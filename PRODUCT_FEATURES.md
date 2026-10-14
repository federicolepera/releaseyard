# ReleaseYard — Top 10 funzionalità di prodotto

Candidati per evolvere l'MVP locale in una piattaforma usabile da più team.
Ordinati per rapporto valore/sforzo. Non è un impegno: è una shortlist ragionata.

| # | Funzionalità | Impatto | Sforzo | Toca |
|---|--------------|---------|--------|------|
| 1 | Audit trail & activity log | Alto | Basso | `internal/dashboard` |
| 2 | Rollback e storico revisioni Helm | Alto | Basso | dashboard + `internal/helmrelease` |
| 3 | Promozione tra ambienti (dev → prod) | Alto | Medio | dashboard + controller |
| 4 | Multi-tenant per namespace/progetto | Alto | Medio | `internal/dashboard/rbac.go`, RBAC chart |
| 5 | Catalogo arricchito + schema dei valori | Alto | Medio | `api/v1alpha1`, `internal/contract`, web |
| 6 | Approval & policy gate per create/promote | Medio | Medio | dashboard + webhook |
| 7 | Verifica supply-chain del chart | Medio | Medio | `internal/chartfetch/puller.go` |
| 8 | Dettaglio Application con health drill-down | Medio | Basso | `application_types.go` + web |
| 9 | Notifiche & integrazione osservabilità | Medio | Basso | dashboard + controller |
| 10 | Onboarding self-service dei cluster | Medio | Medio | `api/v1alpha1/cluster_types.go`, dashboard |

---

## 1. Audit trail & activity log

**Cosa.** Registrare ogni scrittura fatta dal dashboard: attore (utente impersonato),
gruppi, azione, oggetto, esito, timestamp. Esporli in una pagina "Activity" e come
Kubernetes Events.

**Perché.** Il dashboard oggi non logga nulla (`internal/dashboard` non ha `log.`).
Con autenticazione e impersonation è il requisito minimo per operare: chi ha creato,
modificato o cancellato cosa.

**Chi lo vede.** Non "tutti": la visibilità segue la **stessa RBAC della risorsa**,
mai un permesso applicativo a parte. Tre livelli:

| Livello | Contenuto | Chi lo vede | Come si decide |
|---------|-----------|-------------|----------------|
| 1. Per-oggetto | Storia di quella singola Application/Cluster | Chi può leggere quell'oggetto | `SelfSubjectAccessReview` su `get` della risorsa |
| 2. Per-progetto | Feed filtrata sul namespace/progetto | I membri del namespace | RBAC sul namespace (si sposa con la funzionalità #4) |
| 3. Piattaforma/sicurezza | Login falliti, modifiche config OIDC, onboarding/rotazione kubeconfig, azioni admin, uso dell'impersonation | Solo platform admin | Permesso dedicato (es. `cluster-admin` o una risorsa `auditevents`), **non** implicito nell'accesso |

Default consigliato: livello 1–2 a chi ha accesso all'oggetto/namespace, livello 3
solo agli admin. Mai "tutti".

**Dove.** `internal/dashboard/server.go` (middleware sui write handler),
`internal/dashboard/rbac.go` (per l'identità impersonata). Per i livelli 1–2 riusa
gli **eventi Kubernetes** (già namespace-scoped e già filtrati da RBAC su `events`),
senza store nuovo.

**Note.** Il log Kubernetes con impersonation riporta già `user.username`/
`impersonatedUser`, ma non è tamper-evident né fruibile dagli utenti: per il livello
3 (e per una traccia durevole) **inoltra a un sink centrale** (stdout strutturato →
log store, Loki/ES) invece di usare il cluster o il DB del dashboard come archivio
di audit.

---

## 2. Rollback e storico revisioni Helm

**Cosa.** Una vista per Application con le revisioni Helm e un pulsante "rollback"
a una revisione precedente.

**Perché.** Helm tiene già la history; lo status della CR conserva `helmRevision`,
`chartPins` e `applied` (`api/v1alpha1/application_types.go`). È quasi solo UI più
un'azione controller: valore altissimo a costo basso.

**Dove.** Nuovo endpoint `GET /api/applications/{name}/revisions` e
`POST /api/applications/{name}/rollback`; `internal/helmrelease/installer.go` per
l'azione `Rollback`. Attenzione a: pin del digest, `ChartPins`, e a chi ha il
permesso (`applications update` via `SelfSubjectAccessReview`).

---

## 3. Promozione tra ambienti (dev → prod)

**Cosa.** Promuovere la stessa versione/configurazione di un'Application attraverso
gli ambienti, con eventuale approvazione.

**Perché.** Il modello "environment" esiste già (`ApplicationEnvironmentSelection`,
snapshot immutabile dei target). Oggi manca il flusso di promozione: è il collo di
bottiglia pratico per un IDP.

**Dove.** Nuova nozione di "release train"/promozione nel dashboard; riuso della
logica di snapshot in `internal/controller/application_environment*.go`.

---

## 4. Multi-tenant per namespace/progetto

**Cosa.** Scegliere il namespace (progetto) in cui creare l'Application; il RBAC
k8s diventa namespace-scoped e ogni team vede/gestiona solo i suoi progetti.

**Perché.** Oggi le app finiscono nel namespace fisso del dashboard e la capability
è calcolata su `a.namespace` (`internal/dashboard/rbac.go`). Rendendo il namespace
selezionabile e passandolo alla `SelfSubjectAccessReview`, il RBAC nativo fa da
multi-tenancy senza logica applicativa.

**Dove.** `internal/dashboard/rbac.go` (namespace dinamico), `server.go` (route per
progetto), `dist/chart/templates/dashboard/rbac.yaml`. Requisito: un ClusterRole
aggregato per "team" e un modello di namespace come progetto.

---

## 5. Catalogo arricchito + schema dei valori

**Cosa.** Metadati per definizione (titolo, descrizione, icona, categoria,
documentazione), e uno schema dei valori configurabili per generare il form e
validare l'input lato server.

**Perché.** Il contratto attuale valida solo i percorsi dichiarati
(`internal/contract/applicationdefinition.go`); il form è costruito a mano. Uno
schema (JSON Schema / OpenAPI v3) rende il catalogo auto-descrittivo e il form
generico.

**Dove.** `api/v1alpha1/applicationdefinition_types.go`, `internal/contract/`,
`internal/dashboard/web/app.js`.

---

## 6. Approval & policy gate per create/promote

**Cosa.** Regole per cui certe azioni (creare in prod, promuovere, cancellare)
richiedono un secondo attore o soddisfano una policy.

**Perché.** Con RBAC e multi-team, l'approvazione a due mani su ambienti sensibili è
spesso richiesta. Si appoggia a `SelfSubjectAccessReview` + una CR di richiesta
approvazione.

**Dove.** Nuova API di "change request" nel gruppo `platform.releaseyard.io` +
endpoint dashboard. Alternativa più semplice: un **validating admission webhook**
che blocca le promo non approvate (c'è già lo scaffolding webhook nel progetto).

---

## 7. Verifica supply-chain del chart

**Cosa.** Firma/attestazione del chart (cosign/Sigstore), SBOM, e HTTPS-only in
produzione con pin del digest.

**Perché.** Il puller consente HTTP verso host in allowlist e non verifica le firme
(`internal/chartfetch/puller.go`). Il pin del digest esiste già (`ChartPins`); la
verifica della firma chiude il cerchio sulla fiducia.

**Dove.** `internal/chartfetch/puller.go` (hook di verifica dopo il download),
`ApplicationDefinition` (campo policy di firma), valori chart per abilitarla.

---

## 8. Dettaglio Application con health drill-down

**Cosa.** Pagina di dettaglio che mostra le risorse renderizzate e la salute di
Deployment/Pod/StatefulSet/DaemonSet/Job, con navigazione padre→figlio.

**Perché.** I tipi di health esistono già in `application_types.go`
(`ApplicationDeploymentHealth`, `Pod`, `StatefulSet`, ...) e lo status ha
`resources`. Manca soprattutto la UI.

**Dove.** `internal/dashboard/web/applications.html` + `app.js`; nessuna modifica
API necessaria in prima battuta.

---

## 9. Notifiche & integrazione osservabilità

**Cosa.** Notifiche di evento (create/ready/degraded/failed/rollback) verso
Slack/Teams/webhook, e metriche Prometheus del dashboard e delle Application.

**Perché.** Senza osservabilità gli utenti non sanno quando qualcosa si rompe. Il
manager espone già una metrics server; il dashboard no.

**Dove.** Handler di notifica nel controller (`internal/controller/*`) e/o nel
dashboard; config in chart (`values.yaml`).

---

## 10. Onboarding self-service dei cluster

**Cosa.** Creare/ruotare da UI il kubeconfig di un Cluster (Secret di credenziali),
con validazione di raggiungibilità e permessi prima di salvare, e indicatore di
scadenza/rotazione.

**Perché.** Oggi i Secret vanno creati a mano (vedi `hack/`). Con RBAC self-service è
il passo che rende la piattaforma autogestita. Correlato ai "Dubbi" di
`STATO_ATTUALE.md` su credenziali e rotazione cluster.

**Dove.** `api/v1alpha1/cluster_types.go` (stato di validazione/rotazione),
`internal/dashboard` (endpoint di onboarding), chart (RBAC mirato sui Secret).

---

## Menzioni d'onore

- **Valori secret-backed**: permettere override che puntano a Secret, con masking in UI.
- **OIDC configurabile**: claim name per i gruppi, normalizzazione path, sessioni e
  logout/revoca integrate con Keycloak.
- **Ricerca/filtro/paginazione** e auto-refresh nelle liste.
- **Drift detection** dichiarata in UI (la preview target esiste già lato dashboard).
- **Webhook di admission** per lo snapshot environment anche da `kubectl`
  (vedi `ISSUE.md`).
