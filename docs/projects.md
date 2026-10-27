# Project e GitConnection

`Project` è il boundary di un progetto: crea un repository Git privato e lo inizializza
con una struttura Kustomize minima. Nelle prossime iterazioni aggiungerà namespace,
RBAC e sincronizzazione GitOps. `Project` è cluster-scoped; `GitConnection` è namespaced
e contiene la configurazione del provider Git.

L'unico provider implementato è **GitHub**. L'API di `Project` è già provider-neutral:
`spec.repository.connectionRef` punta a una `GitConnection`, e in futuro si potranno
aggiungere altri provider senza cambiare `Project`.

## Cosa viene creato

Alla creazione del Project viene creato un repository privato con:

```text
README.md
kustomization.yaml
apps/
  kustomization.yaml
```

Le applicazioni future vivranno in `apps/<app>/`. Il repository è creato dal Project,
non da un template esterno.

## Prerequisiti

- Il token deve poter creare repository privati per l'utente o l'organizzazione
  indicata in `GitConnection`, e poter scrivere contenuti e ref.
- Conserva il token in un secret manager o in un file protetto. Non inserirlo in
  manifest, Git o comandi che finiscono nella shell history.
- In produzione preferisci una GitHub App o un token fine-grained con il minimo
  scope necessario.

## 1. Crea il Secret del token

Il Secret deve stare nello stesso namespace della `GitConnection` e contenere la
chiave `token`.

```sh
kubectl create secret generic releaseyard-git-acme \
  --from-file=token=/percorso/protetto/token.txt \
  --namespace idp-system
```

## 2. Crea la GitConnection

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: GitConnection
metadata:
  name: acme
  namespace: idp-system
spec:
  provider: github
  tokenSecretRef:
    name: releaseyard-git-acme
  github:
    organization: acme
```

Campi:

| Campo | Note |
|---|---|
| `spec.provider` | Oggi solo `github`; immutabile. |
| `spec.tokenSecretRef.name` | Secret con chiave `token`; immutabile. |
| `spec.github.organization` | Utente o organizzazione GitHub proprietaria dei repository; immutabile. |

Se `organization` coincide con l'utente autenticato, il repository viene creato
nell'account utente; altrimenti viene creato nell'organizzazione indicata.

## 3. Crea il Project

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Project
metadata:
  name: project-sample
spec:
  repository:
    name: project-sample
    connectionRef:
      name: acme
      namespace: idp-system
  description: Repository del progetto esempio
  delivery:
    mode: both
```

- `spec.repository.name` è il nome del repository GitHub. Se vuoto, usa il nome
  del `Project`. È immutabile.
- `spec.repository.connectionRef.namespace` è opzionale. Se vuoto, usa il namespace
  credenziali configurato nel manager (predefinito `idp-system`).
- `spec.description` è opzionale e viene applicata al repository.
- `spec.delivery.mode` è `direct`, `gitops` o `both` e default `direct`. `both` abilita
  la scelta Direct/GitOps nel form di creazione Application.

## 4. Verifica

```sh
kubectl wait --for=condition=RepositoryReady project/project-sample --timeout=5m
kubectl describe project project-sample
kubectl get project project-sample -o yaml
```

Lo stato contiene:

- `status.repository`: ID, owner, nome, URL web, `cloneURL`, `sshURL`, default branch;
- `status.provisioning`: checkpoint usato per non ripetere una creazione ambigua;
- `RepositoryReady=True`: repository creato, inizializzato e verificato.

La `cloneURL` e la `sshURL` sono quelle da usare per Flux o per il bridge PR.

## Comportamento e limiti

- Un repository già esistente con lo stesso nome ma non riconducibile al `Project`
  genera `Conflict`; non viene adottato.
- Eliminare il `Project` **non elimina** il repository GitHub. Ricreare un Project
  con lo stesso nome ha un UID diverso e genera `Conflict`.
- Se la creazione potrebbe essere stata inviata ma l'esito non è noto, il checkpoint
  impedisce un secondo POST e il Project passa a `RecoveryRequired`. Serve un
  intervento operativo; non cancellare e ricreare la risorsa per aggirarlo.
- Un rifiuto definitivo del provider (per esempio permessi o conflitto) produce
  `RepositoryRejected`; il controller ritenta dopo 5 minuti. Un `403` può essere un
  problema di permessi o un secondary rate limit di GitHub.
- Se il repository esiste ma la scrittura della struttura iniziale fallisce, il
  Project passa a `RepositoryIncomplete` e ritenta in modo idempotente.

## Delivery GitOps

Con `spec.delivery.mode: both` (o `gitops`) la dashboard permette di scegliere
GitOps. In quel caso non crea direttamente l'Application: crea una
`ApplicationRequest` durevole nello namespace della dashboard. Il bridge:

1. legge `Project` e `GitConnection`;
2. legge `apps/kustomization.yaml` dal branch default del Project;
3. scrive `apps/<app>/application.yaml` e `apps/<app>/kustomization.yaml`, poi aggiunge `<app>` alle `resources`;
4. crea il branch `releaseyard/<app>-<request-uid-8>`;
5. apre una pull request verso il branch default;
6. aggiorna la request con branch, commit, URL della PR e phase
   `ChangeRequestOpen`.

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: ApplicationRequest
metadata:
  name: project-sample
  namespace: idp-system
spec:
  projectRef:
    name: project-sample
  applicationName: project-sample
  applicationNamespace: releaseyard-team-platform
  requestedBy: admin
  idempotencyKey: ...
  application:
    applicationDefinitionRef:
      name: releaseyard-demo
    clusterRef:
      name: releaseyard-managed-a
status:
  phase: ChangeRequestOpen
  message: Pull request opened
  branch: releaseyard/project-sample-80cf3201
  commitSHA: b8abf0c7a3824935c1c66c72098c65971e09690c
  changeRequestURL: https://github.com/acme/project-sample/pull/1
```

Il bridge è idempotente: se il branch o la PR esistono già, riusa la PR aperta e
non riscrive il commit. Un'Application già presente nel repository o già elencata
in `apps/kustomization.yaml` viene rifiutata con phase `Conflict`/`Rejected`.

## Argo CD Application per Project

Quando `spec.delivery.mode` è `gitops` o `both`, il controller `Project` crea una
sola Argo CD `Application` per Project, nel namespace `argocd`:

```yaml
spec:
  source:
    repoURL: https://github.com/acme/project-sample.git
    targetRevision: main
    path: apps
  destination:
    server: https://kubernetes.default.svc
    namespace: releaseyard-team-platform
```

La sync policy viene lasciata **manuale** di default: il campo
`spec.syncPolicy.automated` non viene impostato. Quando sei sicuro, lo abiliti
da Argo CD e il controller lo preserva nei reconcile successivi.

Namespace e destination sono configurabili con:

```yaml
manager:
  argocd:
    namespace: argocd
    destinationNamespace: releaseyard-team-platform
```

Se esiste già una Application Argo con lo stesso nome ma non è gestita da
ReleaseYard, il Project espone la condition `GitOpsApplicationConflict` e non la
adotta.

## Cosa non è ancora incluso

Questa iterazione non crea namespace o RBAC, non osserva il merge della PR, non
sincronizza automaticamente il merge e non elimina il repository o la Application
Argo alla cancellazione del Project. Sono le prossime slice:

1. namespace e RBAC del progetto;
2. sync automatico del Project usando Argo CD o Flux;
3. update/delete GitOps: oggi il bridge copre solo la creazione;
4. namespace e RBAC della dashboard per i team.

La modalità `rbac.namespaced=true` non è supportata dal controller `Project`.
