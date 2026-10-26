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
# repository, and buggrit's own dogfood workflow get credentials.
gcloud iam workload-identity-pools create github --project $PROJECT \
  --location global --display-name "GitHub Actions"
gcloud iam workload-identity-pools providers create-oidc buggrit-review --project $PROJECT \
  --location global --workload-identity-pool github \
  --display-name "buggrit review workflow" \
  --issuer-uri https://token.actions.githubusercontent.com \
  --attribute-mapping "google.subject=assertion.sub,attribute.repository_owner=assertion.repository_owner,attribute.job_workflow_ref=assertion.job_workflow_ref" \
  --attribute-condition "assertion.repository_owner == 'SnapdragonPartners' && (assertion.job_workflow_ref.startsWith('SnapdragonPartners/buggrit/.github/workflows/review.yml@') || assertion.job_workflow_ref.startsWith('SnapdragonPartners/buggrit/.github/workflows/buggrit.yml@'))"
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

## Roles

Connect to the `buggrit` database as `postgres` (through the proxy, below)
and create the roles:

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
