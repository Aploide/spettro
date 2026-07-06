# Spettro Subscription

Spettro offers a paid subscription plan that provides:

- **Inference credits** — use Spettro-managed models through the `spettro`
  provider without configuring third-party API keys.
- **Multi-model access** — several models available under a single key,
  tiered by the plan (see [Pricing](https://spettro.app/pricing)).
- **Overflow tier** — pro/max plans continue working on a free-tier model
  once the monthly credit budget is exhausted (with automatic retry).

## Signing in

Inside the TUI:

```text
/login
```

This starts a **device-flow** login:

1. Spettro contacts the backend and gets a browser URL.
2. A message in the chat shows the URL. Copy it.
3. Open the URL in your browser and sign in.
4. Once authenticated, Spettro receives an `ep_` API key and saves it
   encrypted to `~/.spettro/keys.enc`.

The flow polls the backend every 2 seconds until you complete the browser
step or the session expires.

You can also sign in directly from the [onboarding wizard](onboarding.md)
by selecting **"Sign in to your Spettro subscription"** as your provider.

## Signing out

```text
/logout
```

This:

- Removes the saved `ep_` API key from the encrypted store.
- Clears the Spettro Subscription models from the provider manager.
- Stops any in-flight inference through the Spettro proxy.

You can sign back in at any time with `/login`.

## Active model list

Once signed in, Spettro fetches the models available on your plan from
`GET /v1/models`. These appear in the model selector under the **Spettro**
provider group, labelled with your plan tier.

The model list is refreshed in the background periodically so new models or
plan upgrades are picked up without re-logging.

## Plan and credit info

When signed in, the top bar of the TUI shows your plan name, status, and
credit usage.

```
Spettro (Pro)
```

If the plan is in a special state (e.g. past due, cancelled), the status
is shown alongside the plan name.

## Overflow rate limiting

When a pro or max account exhausts its monthly credit budget, inference
continues on a free-tier model — but at a reduced rate. The Spettro backend
returns `429 Too Many Requests` with a `Retry-After` header. The provider
manager transparently waits out these delays and retries, so you never see
a 429 error in the chat. You may notice slower response times when
approaching or exceeding your credit limit.

The default retry delay is 7 seconds when no `Retry-After` header is
present.

## Configuration

The Spettro Subscription state is stored in:

| Data | Location |
|------|----------|
| API key (`ep_...`) | `~/.spettro/keys.enc` (encrypted) |
| Email / plan / status (cache) | `~/.spettro/config.json` (`spettro_email`, `spettro_plan`, `spettro_plan_status` fields) |

The cached plan info is displayed immediately on startup before the network
refresh completes. It is updated asynchronously in the background.

## API

The Spettro backend (overridable via the `SPETTRO_API_URL` environment
variable) exposes:

| Endpoint | Purpose |
|----------|---------|
| `POST /auth/initiate` | Register a login session, returns a browser URL |
| `GET /auth/poll/:session` | Poll until the user signs in, returns an `ep_` key |
| `GET /v1/models` | List models available on the user's plan |
| `GET /v1/account` | Plan, status, and credit usage |
| `POST /v1/chat/completions` | OpenAI-compatible inference (used by the provider manager) |

The default base URL is `https://api.spettro.app`.

## Pricing

See [https://spettro.app/pricing](https://spettro.app/pricing) for current
plans, credit limits, and model availability.
