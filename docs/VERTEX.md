# Google Vertex AI (Claude) Configuration Guide

Sub2API supports Anthropic Claude models served through Google Vertex AI. This guide walks you through provisioning a GCP service account and registering a Vertex account in the admin dashboard.

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Step 1 — Enable Vertex AI and pick a region](#step-1--enable-vertex-ai-and-pick-a-region)
- [Step 2 — Request access to Anthropic models](#step-2--request-access-to-anthropic-models)
- [Step 3 — Create a service account](#step-3--create-a-service-account)
- [Step 4 — Add the Vertex account in Sub2API](#step-4--add-the-vertex-account-in-sub2api)
- [Model availability](#model-availability)
- [Differences from Bedrock](#differences-from-bedrock)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

- A Google Cloud project with billing enabled
- `gcloud` CLI (optional but convenient) or admin access to the GCP Console
- The ability to create service accounts and download JSON keys in your project

## Step 1 — Enable Vertex AI and pick a region

1. In the GCP Console, open **APIs & Services → Library** and enable **Vertex AI API** for your project.
2. Pick the region you want to serve from. Vertex Anthropic-Claude availability varies by model; common choices:
   - `us-east5` — broadest model coverage today
   - `europe-west1` / `europe-west4` — Europe
   - `asia-southeast1` — APAC
   - `global` — multi-region routing (host is `aiplatform.googleapis.com` without a region prefix)

   Sub2API stores **one region per account**. If you want multiple regions, create one Sub2API account per region.

## Step 2 — Request access to Anthropic models

Anthropic models on Vertex are publisher-gated. In **Vertex AI → Model Garden → Anthropic**, click **Enable** on every Claude model you intend to use (Opus / Sonnet / Haiku as needed). For first-time access GCP may require you to request quota — check **IAM & Admin → Quotas** and bump the relevant per-region request-per-minute quotas if you expect production load.

## Step 3 — Create a service account

```bash
PROJECT_ID="my-gcp-project"

# Create the service account
gcloud iam service-accounts create sub2api-vertex \
  --project="$PROJECT_ID" \
  --display-name="Sub2API Vertex AI"

# Grant it the Vertex AI User role
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:sub2api-vertex@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/aiplatform.user"

# Download the JSON key
gcloud iam service-accounts keys create ./sub2api-vertex-key.json \
  --iam-account="sub2api-vertex@$PROJECT_ID.iam.gserviceaccount.com"
```

The minimum required role is `roles/aiplatform.user`. Treat the downloaded JSON key like a password — Sub2API stores it encrypted in the credentials JSONB column and **never echoes it back in the UI**.

## Step 4 — Add the Vertex account in Sub2API

1. In the admin dashboard, go to **Accounts → Create Account**.
2. Choose platform **Anthropic**, then in the category buttons pick **Vertex AI**.
3. Fill the form:
   - **Service Account JSON** — paste the entire JSON key from step 3
   - **GCP Project ID** — typically the `project_id` field inside the JSON
   - **Region** — the region you picked in step 1 (or `global`)
4. Optionally configure model whitelist / mapping, account-level quota, and pool mode (same controls as Bedrock and API-Key accounts).
5. Save the account, then click **Test Connection**. Sub2API will fetch a fresh access token, send a 1-token `:rawPredict` request, and surface the response.

## Model availability

Sub2API ships a default mapping that resolves common Anthropic model names to Vertex's `@<date>` form:

| Request name | Vertex model ID |
|---|---|
| `claude-opus-4-1` | `claude-opus-4-1@20250805` |
| `claude-opus-4` | `claude-opus-4@20250514` |
| `claude-sonnet-4-5` | `claude-sonnet-4-5@20250929` |
| `claude-sonnet-4` | `claude-sonnet-4@20250514` |
| `claude-haiku-4-5` | `claude-haiku-4-5@20251001` |
| `claude-3-7-sonnet` | `claude-3-7-sonnet@20250219` |

If you need a model not in this list (or a different `@date` revision), add a per-account **model_mapping** entry. Account-level mappings always take precedence over the default table.

## Differences from Bedrock

| Aspect | AWS Bedrock | Google Vertex AI |
|---|---|---|
| Auth | AWS SigV4 (Access Key + Secret) or Bedrock API Key | Service Account JSON → OAuth2 access token (auto-refreshed) |
| Model ID format | `us.anthropic.claude-…-v1:0` (region-prefixed) | `claude-…@<date>` |
| Streaming wire format | AWS EventStream (binary frames) | Standard SSE (`data: {…}\n\n`) |
| Region semantics | Cross-region inference profile prefix is auto-adjusted | Region is part of the URL host; `global` uses no region prefix |
| `count_tokens` endpoint | Not supported (returns 404) | Not supported (returns 404) |

## Troubleshooting

**`vertex: gcp_project_id not configured` or `gcp_region not configured`**
The account credentials map is missing one of the required fields. Re-open the account in the admin UI and confirm both Project ID and Region are set.

**`vertex: parse service account JSON: …`**
The JSON key is malformed or the private key block is invalid. Re-download a fresh key from GCP and paste the entire file contents (including the leading `{` and trailing `}`).

**`API returned 403: Permission … denied on resource …`**
The service account lacks `roles/aiplatform.user`, or it is bound to a different project. Re-check the IAM binding and make sure the SA email matches the project it is granted on.

**`API returned 404: Publisher Model …`**
The requested model is not enabled in the chosen region. Either pick a different region (see step 1) or override the model mapping for this account to point at a region-supported `@<date>` ID.

**`API returned 429: Quota exceeded …`**
Per-region Vertex quota is throttling you. Request a quota increase in **IAM & Admin → Quotas** and look for "Online prediction requests per minute per base model per region" matching your model and region.

**Token refresh stops working after credential rotation**
Sub2API keys its token cache by SA-JSON contents, so saving a new SA JSON in the edit modal automatically invalidates the cached token. If the access token is still failing, confirm the new key is valid by running `gcloud auth activate-service-account --key-file=...` locally.
