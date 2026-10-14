# Stato attuale di ReleaseYard

_Aggiornato all'8 ottobre 2026. Questo documento descrive l'MVP locale, non una versione pronta per la produzione._

## Implementato

- **Cluster**: risorsa cluster-scoped che legge il kubeconfig da un Secret nel namespace delle credenziali del manager e verifica l'autenticazione verso il cluster gestito. La condizione `Connected` indica l'esito dell'ultimo probe, non la salute del cluster o dei workload.
- **ApplicationDefinition**: contratto cluster-scoped con repository ChartMuseum HTTP(S), nome e versione fissata del chart, namespace di destinazione, defaults e percorsi di valori configurabili. Il controller valida la coerenza interna, scarica l'archivio senza interpretarne valori o template, verifica lo SHA-256 indicato nell'indice e pubblica `Validated` e digest. `Validated=True` **non** garantisce che Helm possa installare il chart.
- **Application**: risorsa namespaced che riferisce una `ApplicationDefinition` cluster-scoped e un `Cluster`. Accetta solo override dichiarati nel contratto, controlla tipi, limiti e valori obbligatori, quindi combina i defaults della definition con gli override. I riferimenti e la destinazione già associata non vengono cambiati silenziosamente.
- **Distribuzione**: il controller richiede una definition validata per la generazione corrente, riscarica il chart e confronta il digest prima di passare **quegli stessi byte** a Helm 3.22.0. Registra nello status il binding al cluster, i digest già osservati per ciascuna versione, il tentativo e la revisione applicata. Installa o aggiorna la release nel namespace **già esistente** del cluster selezionato; il nome della release coincide con quello dell'Application.
- **Ownership e rimozione**: una ConfigMap di claim nel cluster gestito e i label della release impediscono l'adozione automatica di una release altrui. Il finalizer prova a disinstallare la release e rimuovere la claim prima di completare la cancellazione dell'Application. In caso di destinazione irraggiungibile o ownership ambigua, si ferma invece di cancellare alla cieca.
- **Perimetro Helm dell'MVP**: sono accettati solo chart autosufficienti **senza** dipendenze, `values.schema.json`, hook o CRD. Repository OCI, adozione di release preesistenti, rollback e riparazione automatica di revisioni Helm pending non sono implementati. L'ambiente previsto è fidato e amministrato da un solo team, non multi-tenant.
- **Demo locale**: `hack/setup-kind-workloads.sh` crea o riusa `releaseyard-a` e i due managed `releaseyard-managed-a/b`; installa ChartMuseum nel management e pubblica `releaseyard-demo:0.1.0`. Il campione Application è **opt-in** ed escluso da `config/samples/kustomization.yaml`. README e configurazione VS Code descrivono il debug locale senza distribuire il manager come Pod.
- **Dashboard - accesso locale**: la dashboard parte senza alcun provider OIDC e si entra con l'utente `admin` e la password generata all'installazione. Il Job di bootstrap (hook `pre-install,pre-upgrade` nella chart, oppure Job Kustomize nel dev) crea una sola volta il Secret `*-dashboard-local-admin` con `password-hash` (bcrypt cost 12) e `initial-password`; il Pod monta solo l'hash, mai il plaintext. La configurazione OIDC e la scelta local/OIDC al login sono rimandate a fasi successive.

## Verificato finora

- `make lint-fix`, `make test` (compresi i test envtest), build Go, build Kustomize e `git diff --check` erano passati dopo le modifiche al controller.
- Smoke test reale: `Application/releaseyard-demo-sample` in `idp-system` su `kind-releaseyard-a` ha raggiunto **`Ready=True`** e installato la release nel namespace `applications` di `kind-releaseyard-managed-a` alla **revisione 1**.
- Upgrade reale: cambiando `replicaCount` da 2 a 3 e `message` in `Upgrade verificato ReleaseYard`, Helm ha raggiunto la **revisione 2, deployed**. Il Deployment aveva **3/3 repliche pronte** e la ConfigMap della pagina conteneva il nuovo messaggio.
- Delete reale: eliminando l'Application, il finalizer ha completato il cleanup. Sono risultati assenti Application, release/storia Helm, Secret Helm, Deployment, Service, ConfigMap della pagina e ConfigMap di claim. La definition è rimasta `Validated=True`, il Cluster `Connected=True`; nel managed-b il namespace `applications` è rimasto assente.
- Le prove hanno usato temporaneamente manager e port-forward locali, poi fermati entrambi. **L'Application campione e la release ora non esistono più**; restano il namespace `applications` nel managed-a, la definition, il Cluster e il chart pubblicato su ChartMuseum. Per ripetere la prova con F5 serve il port-forward documentato nel README.
- Le modifiche dell'Application nel repository erano ancora **non committate** al termine della prova.

## Dubbi e cose da sistemare

1. **Altri percorsi di aggiornamento**: sono stati provati live l'upgrade dei valori e il delete normale, ma non il cambio di versione del chart nella definition né i casi di errore durante upgrade e cleanup.
2. **Recupero da interruzioni**: binding, tentativi e checkpoint di cleanup sono progettati per fermarsi in sicurezza e riprendere; mancano prove live di arresto del manager durante un'azione Helm, Secret Helm danneggiati, claim scomparsa e cluster irraggiungibile durante la cancellazione. Nei casi ambigui può essere necessaria una procedura manuale per sbloccare una release pending o un finalizer.
3. **Credenziali e autorizzazione**: nella demo i kubeconfig managed hanno privilegi amministrativi. La Role del manager limita la lettura del Secret al namespace delle credenziali, ma non è definito un modello di autorizzazione multi-tenant né un RBAC minimo per le operazioni Helm e per le risorse dei chart.
4. **Identità del chart e rotazione cluster**: ripubblicare byte diversi con lo stesso repository/nome/versione viene rifiutato dalle Application che hanno già fissato quel digest; cambiare endpoint o CA del cluster richiede un recupero amministrativo. Mancano procedure operative documentate per questi casi.
5. **Portata della validazione**: `ApplicationDefinition` non apre né renderizza il chart per scelta; la verifica che il pacchetto sia utilizzabile avviene solo quando `Application` tenta la distribuzione. Chart e template restano input fidati con accesso alle API del cluster secondo i privilegi del kubeconfig: non sono una sandbox per autori non fidati.

L'installazione, l'upgrade dei valori e la rimozione normali sono quindi dimostrati nel Kind locale; i punti sopra riguardano comportamento e operatività fuori dal percorso felice.
