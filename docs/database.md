# The optional database

buggrit runs without a database. With one, it can share a rate limit across
repositories and keep run telemetry ([design §6](design.md#6-history-and-state),
"The optional database"). This is how SnapdragonPartners' is set up: Cloud
SQL for PostgreSQL, reached from Actions through the Cloud SQL Auth Proxy
with Workload Identity Federation and IAM database login, so there are no
keys or passwords ([design §3](design.md#3-trigger-packaging-permissions),
"Database"; [§10](design.md#10-security-proportionate-to-the-trust-model)).

| | |
|---|---|
| Project | `buggrit` (number `663301162144`) |
| Instance | `buggrit:us-east1:buggrit-db`, PostgreSQL 18, SSL only, `cloudsql.iam_authentication=on`, no authorized networks |
| Database | `buggrit` |
| Runtime identity | `buggrit-runner@buggrit.iam.gserviceaccount.com`, database user `buggrit-runner@buggrit.iam` |
| Roles | `buggrit_owner` owns the schemas (migrations); `buggrit_runtime` reads and writes rows |

## Google Cloud

```sh
PROJECT=buggrit
NUMBER=663301162144
SA=buggrit-runner@buggrit.iam.gserviceaccount.com

gcloud services enable --project $PROJECT \
  sqladmin.googleapis.com iamcredentials.googleapis.com iam.googleapis.com sts.googleapis.com

gcloud iam service-accounts create buggrit-runner --project $PROJECT \
  --display-name "buggrit review runs"
for role in roles/cloudsql.client roles/cloudsql.instanceUser; do
  gcloud projects add-iam-policy-binding $PROJECT --member "serviceAccount:$SA" --role $role --condition None
done

# Only buggrit's reusable workflow, called from a SnapdragonPartners
# repository, and buggrit's own dogfood workflow get credentials. A job in a
# reusable workflow is named by job_workflow_ref; a plain workflow's token
# names it by workflow_ref and may lack job_workflow_ref, hence has().
gcloud iam workload-identity-pools create github --project $PROJECT \
  --location global --display-name "GitHub Actions"
gcloud iam workload-identity-pools providers create-oidc buggrit-review --project $PROJECT \
  --location global --workload-identity-pool github \
  --display-name "buggrit review workflow" \
  --issuer-uri https://token.actions.githubusercontent.com \
  --attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --attribute-condition "assertion.repository_owner == 'SnapdragonPartners' && ((has(assertion.job_workflow_ref) && assertion.job_workflow_ref.startsWith('SnapdragonPartners/buggrit/.github/workflows/review.yml@')) || (assertion.repository == 'SnapdragonPartners/buggrit' && assertion.workflow_ref.startsWith('SnapdragonPartners/buggrit/.github/workflows/buggrit.yml@')))"
gcloud iam service-accounts add-iam-policy-binding $SA --project $PROJECT \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/$NUMBER/locations/global/workloadIdentityPools/github/*"

gcloud sql databases create buggrit --instance buggrit-db --project $PROJECT
gcloud sql users create buggrit-runner@buggrit.iam --instance buggrit-db --project $PROJECT \
  --type cloud_iam_service_account
# The operator who runs migrations.
gcloud sql users create dan@snapdragonpartners.com --instance buggrit-db --project $PROJECT \
  --type cloud_iam_user
```

Org variables (Settings → Secrets and variables → Actions → Variables),
none of them secret:

| Variable | Value |
|---|---|
| `BUGGRIT_CLOUDSQL_INSTANCE` | `buggrit:us-east1:buggrit-db` |
| `BUGGRIT_GCP_WIF_PROVIDER` | `projects/663301162144/locations/global/workloadIdentityPools/github/providers/buggrit-review` |
| `BUGGRIT_GCP_SERVICE_ACCOUNT` | `buggrit-runner@buggrit.iam.gserviceaccount.com` |
| `BUGGRIT_RATE_LIMITS` | the shared rate limits, below |

### Shared rate limits

`BUGGRIT_RATE_LIMITS` names a bucket for each model endpoint whose limit
every repository shares (design §9, "Shared rate limit"). A stage uses a
bucket when its `base_url` and `api_key_env` match. For LunaRoute, whose
key allows two requests in flight:

```json
{"lunaroute": {"base_url": "https://gw.lunaroute.com/v1", "api_key_env": "LUNAROUTE_API_KEY", "max_concurrency": 2}}
```

`tokens_per_minute` is optional, and so is `max_wait` (how long a call
waits for a lease before the run goes on with only its own limit; a
quarter of `pass_deadline` by default). Each run sets its buckets to these
limits, so to change one, edit the variable. The key itself stays in its
secret.

## Roles

Creating roles needs the built-in `postgres` user, which logs in with its
password, so this one connection goes through a proxy without
`--auto-iam-authn` (the migration proxy, below, logs in IAM users only).
Keep the password in `~/.pgpass` rather than on the command line (if
`postgres` has none, set one with `gcloud sql users set-password postgres
--instance buggrit-db --project buggrit --prompt-for-password`):

```sh
cloud-sql-proxy --gcloud-auth --port 5434 buggrit:us-east1:buggrit-db &
psql 'host=127.0.0.1 port=5434 user=postgres dbname=buggrit sslmode=disable'
```

Then create the roles:

```sql
CREATE ROLE buggrit_owner NOLOGIN;
CREATE ROLE buggrit_runtime NOLOGIN;
GRANT CREATE ON DATABASE buggrit TO buggrit_owner;
GRANT buggrit_owner TO "dan@snapdragonpartners.com";
GRANT buggrit_runtime TO "buggrit-runner@buggrit.iam";
```

Then, as the operator, create the schemas and the runtime role's
privileges, including on tables later migrations add:

```sql
SET ROLE buggrit_owner;
CREATE SCHEMA IF NOT EXISTS buggrit;
CREATE SCHEMA IF NOT EXISTS bucket;
GRANT USAGE ON SCHEMA buggrit, bucket TO buggrit_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA buggrit, bucket
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO buggrit_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA buggrit, bucket
  GRANT USAGE, SELECT ON SEQUENCES TO buggrit_runtime;
```

## Migrations

Runs never change the schema. After a release whose notes say it needs a
migration, and before callers run it, the operator applies it through the
proxy as `buggrit_owner`:

```sh
cloud-sql-proxy --auto-iam-authn --port 5433 buggrit:us-east1:buggrit-db &
export BUGGRIT_DATABASE_URL='postgres://dan%40snapdragonpartners.com@127.0.0.1:5433/buggrit?sslmode=disable&options=-c%20role%3Dbuggrit_owner'
buggrit db migrate
buggrit db status
```

A binary needs each schema at its version or newer, so migrating ahead of
older pinned callers is safe. `db migrate` leaves a newer schema alone, and
refuses a dirty one (a migration that failed partway), which needs fixing
by hand.

## Reading the data

To look at run telemetry ([telemetry.md](telemetry.md)), connect through
the proxy as yourself and read as `buggrit_owner`, in read-only
transactions. A proxy that logs in IAM users can't also take
`--gcloud-auth`, so pass it your access token for both:

```sh
TOKEN="$(gcloud auth print-access-token)"
cloud-sql-proxy --auto-iam-authn --token "$TOKEN" --login-token "$TOKEN" --port 5433 buggrit:us-east1:buggrit-db &
PGOPTIONS='-c role=buggrit_owner -c default_transaction_read_only=on' \
  psql 'host=127.0.0.1 port=5433 user=dan@snapdragonpartners.com dbname=buggrit sslmode=disable' \
  -v run=<uuid> -f query.sql
```
