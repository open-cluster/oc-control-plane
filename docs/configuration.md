# Configuration

Configuration is environment variables, validated at startup: the first problem refuses
the start and names the offending variable. No environment value ever carries a
credential — every secret is a FILE the variable names — and no error quotes a secret
file's contents. `README.md` carries the variable table with each one's meaning; this
page groups them by what they enable.

| To enable | Set |
| --- | --- |
| The process at all | `OC_HTTP_ADDRESS`, `OC_PLACEMENTS`, and `OC_DEFAULT_PLACEMENT` or `OC_PLACEMENT_ASSIGNMENTS` |
| Relays | `OC_RELAY_ADDRESS` + `OC_RELAY_SPKI_PINS` |
| The operator surface | `OC_OPERATOR_ADDRESS` + `OC_OPERATOR_TOKEN_FILE` + `OC_OPERATOR_TOKEN_ORGANIZATION` |
| Browser sign-in | `OC_OPERATOR_PUBLIC_URL`, `OC_OPERATOR_CONSOLE_URL`, `OC_OPERATOR_ALLOWED_ORIGINS`, `OC_SEALING_KEY_FILE` |
| Alert intake | `OC_INTAKE_ADDRESS` (+ `OC_INTAKE_PUBLIC_URL` so webhook endpoints render whole) |
| Credential-bearing integrations (Slack) | `OC_SEALING_KEY_FILE` — without it, a catalog serving such a type refuses to start the operator surface |
| GitHub | `OC_GITHUB_APP_ID` + `OC_GITHUB_APP_PRIVATE_KEY_FILE` (both or neither); `OC_GITHUB_API_URL` for Enterprise hosts |
| Investigations | `OC_MODEL_PROVIDER`, `OC_MODEL_NAME`, `OC_MODEL_KEY_FILE`, `OC_MODEL_CONSENTED_PROVIDERS` (+ optional `OC_MODEL_EFFORT`, `OC_MODEL_BASE_URL`, `OC_MODEL_SPEND_CEILING_CENTS`, `OC_INVESTIGATION_WINDOW_LEAD`) |

## Rules worth knowing

- **Placements.** `OC_PLACEMENTS` maps names to DSN files; assignments pin organizations
  to placements; the default serves the rest. An unassigned organization with no default
  is a hard error, never a fallback.
- **Half a credential is refused.** A GitHub app id without its key, a model provider
  without its model name or key — each refuses startup while whoever set the first
  variable is still reading.
- **Consent is explicit.** `OC_MODEL_CONSENTED_PROVIDERS` lists the providers an
  investigation's material may be sent to; nothing listed permits nothing, including the
  configured provider itself.
- **Vendor URLs** (`OC_SLACK_API_URL`, `OC_GITHUB_API_URL`, `OC_MODEL_BASE_URL`) must be
  https except on loopback, because a credential is presented to whatever answers there.
- **The sealing key** is 32 bytes, raw or base64, in a file. Rotating it makes stored
  credentials unopenable; each then verifies as failed with "paste it again to replace
  it" — replace, don't panic.
- **The spend ceiling** (`OC_MODEL_SPEND_CEILING_CENTS`, default 500) is a hard
  per-investigation cap in whole cents. A reached ceiling ends the investigation as an
  honest partial conclusion labeled `stoppedBy: "spend"`; the ceiling can be raised but
  never removed.
- **The window lead** (`OC_INVESTIGATION_WINDOW_LEAD`, default `2h`) widens every
  investigation's window backwards before the incident began, because the change that
  caused an incident usually landed before it fired. Every tool read is clamped inside
  the widened window.
