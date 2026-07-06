# First-Launch Onboarding

When Spettro starts with **no provider API keys configured** and **no local
endpoints**, it presents a guided onboarding wizard so you can get connected
and start using the tool immediately — no config file editing required.

The onboarding flow replaces the usual chat view until you complete it or
dismiss it manually.

## When onboarding appears

Onboarding triggers when **all** of these are true:

- No Spettro Subscription is signed in (`spettro` provider key is empty).
- No provider API keys are stored (Anthropic, OpenAI, etc. are empty).
- No local endpoints are configured.
- No cached model catalog is available.

If you already have a working configuration, onboarding is skipped and the
normal TUI starts immediately.

## The flow

### Step 1: Choose a provider and model

```
  To start, let's choose a provider and model.

> anthropic
  claude-sonnet-4-20250514    popular
  claude-sonnet-4-5-20250514  popular
  claude-4-haiku-20250514     fast
  claude-opus-4-20250514      powerful

  openai
  gpt-5-mini                  fast
  ...
```

- Type to **filter** by provider name or model name.
- Use `↑` / `↓` to navigate.
- Press `Enter` to select.

At the top of the list you will also see **Sign in to your Spettro
subscription** — selecting this starts the device-flow login instead of asking
for an API key (see [Subscription](subscription.md)).

### Step 2: Enter your API key

```
  Enter your Anthropic key.

> sk-ant-xxxxxxxxxxxxx

  This will be written to your global configuration:
  ~/.spettro/keys.enc

  enter submit  •  esc back
```

- Paste or type your API key.
- Press `Enter` to submit.
- Press `Esc` to go back to the provider picker.
- Press `Ctrl+C` to quit.

The key is verified against the provider's API before it is saved. It is stored
encrypted (AES-GCM) in `~/.spettro/keys.enc`, never in plaintext.

### Step 3: Verification

While the key is being tested, a progress bar animates:

```
  Verifying your Anthropic Key...

  ◈ ▐████████████████████████████████████▌
```

If verification succeeds, the onboarding closes and a system message confirms
the connection:

```
  connected Anthropic ✓ — ready to use claude-sonnet-4-20250514
```

### Step 4 (error): Retry

If the key is rejected, an error screen shows the reason:

```
  Failed to verify your Anthropic key.

  key rejected (401)

  enter / esc — try again
```

Press `Enter` or `Esc` to return to the key entry and correct the value.

## Dismissing onboarding

You can **always** quit Spettro during onboarding with `Ctrl+C`. On the
provider picker screen, `Ctrl+C` quits. On the key entry screen, `Ctrl+C`
also quits.

There is no "skip" button — if you need to configure something that the
wizard doesn't cover (a local endpoint, for example), you can still do so
after onboarding via `/connect`. Onboarding simply won't appear if any
provider key is already configured.

## After onboarding

Once you complete onboarding:

1. Your API key is saved encrypted.
2. The selected provider and model become active.
3. The normal chat view starts — you are ready to send prompts.

You can always switch models later with `/models`, add another provider with
`/connect`, or sign in to a Spettro Subscription with `/login`.

## Configuration file paths

The onboarding wizard writes to:

- `~/.spettro/keys.enc` — encrypted API key.
- `~/.spettro/config.json` — active provider and model.

Both files are created with restricted permissions (`0o600` / `0o700`).