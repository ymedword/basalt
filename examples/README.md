# Examples: daily agents, templates and shared assets

Five daily agents and eight templates built on the same Basalt platform, plus the shared assets they need. Every directory here is a **portable baseline**: no instance UIDs, no credentials, no channel bindings. You copy a directory into a workspace, follow its `SETUP.md`, and bind your own model and delivery channel.

Nothing here is imported automatically, and nothing here widens an existing workspace: importing an example never changes your `mcp_servers.yaml`, your command allowlist or your tool policy.

## Daily agents

| Directory | What it produces | External service | In-repo evidence |
| --- | --- | --- | --- |
| [daily/github-radar/](daily/github-radar/) | Star deltas and new projects for a topic, after de-duplicating against the previous run | GitHub MCP server + PAT | Fixture replay: de-duplication, star delta, first run without baseline |
| [daily/repo-digest/](daily/repo-digest/) | Pull request / issue / release counts with follow-up items, delivered by the schedule | GitHub MCP server + PAT | Fixture replay: counts, "to be confirmed" fields, cursor advance |
| [daily/biz-daily/](daily/biz-daily/) | Business metrics from a read-only SQLite sample, with data-quality counts | DBHub over SQLite | Fixture replay: missing / duplicate / invalid rows counted separately |
| [daily/docs-diff/](daily/docs-diff/) | Added / modified / deleted documents with a content hash each | Bundled `docs-readonly` MCP server | Fixture replay: three change sets, content hashes, first run without baseline, unchanged run still reports |
| [daily/industry-brief/](daily/industry-brief/) | Briefing with a source table of URL, title, date and access time | Built-in `web_search` (DeepSeek Search API) + Fetch MCP | Fixture replay: unfetched URLs stay leads only; the search and Fetch channels are both wired and invoked (release e2e) |

Every agent writes a machine-readable result next to its report, so the numbers in the report can be re-computed from the JSON instead of being estimated by the model.

## Templates

| Directory | Domain | Tier | Needs |
| --- | --- | --- | --- |
| [templates/weather-brief/](templates/weather-brief/) | Personal admin | A | HTTP only, no credentials |
| [templates/arxiv-tracker/](templates/arxiv-tracker/) | Research | A | HTTP only, no credentials |
| [templates/policy-watch/](templates/policy-watch/) | Policy monitoring | A | HTTP to named official sources |
| [templates/ticket-insights/](templates/ticket-insights/) | Support / product | A | DBHub over the sample SQLite database |
| [templates/onboarding-buddy/](templates/onboarding-buddy/) | HR onboarding | A | Bundled `docs-readonly` MCP server |
| [templates/dependency-watch/](templates/dependency-watch/) | Security maintenance | A | HTTP to OSV |
| [templates/web-watch/](templates/web-watch/) | Ops testing | A\* | HTTP + Playwright browser |
| [templates/funding-watch/](templates/funding-watch/) | Startup research | B | Brave Search + Fetch, requires a Brave API key |

Tier definitions:

- **A** — complete agent directory with fixtures, expected results and a README (templates carry no `SETUP.md`; that file belongs to the demos); the agent loads, its built-in tools resolve, and the MCP tools it declares are a subset of what the target server actually lists; replayable from fixtures without credentials.
- **A\*** — same as A, but an optional dependency (the Playwright browser) must be present. Without it the template stays A\*; it is never relabelled as B.
- **B** — a configuration recipe with dependencies, credentials and known limits. It loads, but it is **not** claimed to run: no fixture replay, no expected output.

## Prerequisites

Examples run outside the Basalt binary, so they need their own tools. None of this is required to build or run Basalt itself.

| Purpose | Requirement |
| --- | --- |
| Demo scripts and the sample database | Python 3.12 (`py -3` on Windows, `python3` elsewhere) |
| Fetch MCP server | Python package `mcp-server-fetch` installed for that same interpreter — not vendored here |
| GitHub / Brave / Playwright / DBHub MCP servers | Node 24 plus the corresponding npm package — not vendored here |
| Bundled read-only MCP server | Go 1.27 toolchain, or a prebuilt binary |
| Model | Any endpoint and model name you configure; the examples ship a visible placeholder, not a working model name |

MCP servers are separate processes. Their network access is decided by each server, not by Basalt; see the "External access" section below.

## Run a daily agent

1. Copy the directory into the workspace, keeping the agent name:

   ```sh
   cp -r examples/daily/github-radar .basalt/agents/github-radar
   ```

2. Read that directory's `SETUP.md` and fill in what it lists — usually the model name, the model connection, the notification channel and the MCP server path or environment variable. Credentials stay in environment variables; the configuration references them as `${VAR}` placeholders only, never as values.
3. Add the MCP server command to your workspace's command allowlist yourself. The examples declare what they need; they never widen the allowlist for you.
4. Start the management server and enable the agent:

   ```sh
   basalt serve
   ```

5. To run a schedule once without waiting for its time, use the console's run action, or call the management endpoint with your admin token:

   ```sh
   curl -X POST -H "Authorization: Bearer $BASALT_ADMIN_TOKEN" \
     http://127.0.0.1:8080/api/v1/schedules/<schedule-id>/run
   ```

A manual run is recorded with `trigger=manual`. It is evidence of a working agent, but it is not evidence that the schedule fired on its own.

## Credentials

Only one credential is required by the demos: a GitHub personal access token, shared by `daily/github-radar` and `daily/repo-digest`. It does not ship here, and `basalt demo install` never accepts or writes one: the snippets it prints reference `${VAR}` only, and Basalt's configuration gate refuses to save a server whose referenced variable is missing or empty. The full walk-through lives in each demo's `SETUP.md`, so a copied workspace stays self-contained; this section is the shared summary.

`daily/industry-brief` needs **no key of its own**. Its search runs through the built-in `web_search` tool, which reuses the key of the DeepSeek model connection you bind to that agent, and its page bodies come from the Fetch MCP server. Under the `strict` execution profile, add `api.deepseek.com` to the network allowlist on the console's execution-environment page — `web_search` is one of Basalt's own outbound calls, so it is judged like `http_get`. The `funding-watch` template (tier B) still describes a Brave Search recipe and its own `BRAVE_API_KEY`; it is a configuration reference, not part of any demo's setup.

### GitHub personal access token

Used by `daily/github-radar` and `daily/repo-digest`.

1. Open <https://github.com/settings/personal-access-tokens/new> (Settings → Developer settings → Personal access tokens → Fine-grained tokens → Generate new token).
2. Resource owner: your account (an organization may require approval). Repository access: **Public repositories** — read-only over any public repository, with no per-repository selection. Both demos read public metadata, file contents, issues, pull requests and releases; public data needs no extra permission. Add `Contents: read` / `Issues: read` / `Pull requests: read` only if you point them at private repositories (`Metadata: read` is attached by GitHub).
3. Expiration: the form defaults to 30 days and accepts 1–366; "no expiration" is allowed but an organization or enterprise maximum-lifetime policy can block it.
4. Keep the value in the environment of the Basalt process only (`export GITHUB_PERSONAL_ACCESS_TOKEN=...`). Never in `mcp_servers.yaml`, `AGENT.md`, a script or a commit.
5. Start `github-mcp-server` with `--read-only` (`GITHUB_READ_ONLY=1` in Docker), so write tools are never offered, and optionally `--toolsets repos,issues,pull_requests`. That is a server-side limit and a separate layer from the token scope; use both.
6. Verify: `curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $GITHUB_PERSONAL_ACCESS_TOKEN" https://api.github.com/rate_limit` prints `200`. A `401` means the token expired or was revoked.

The token itself is free; there is no plan to buy.

### Brave Search API key (optional; `funding-watch` template only)

No demo uses this key. It is listed here because the tier B `funding-watch` template's recipe references `${BRAVE_API_KEY}`; that template's own README carries the checklist.

1. Subscribe to the **Search** plan at <https://brave.com/search/api/> — the MCP server uses Search, while `Answers` is a different product. A credit card is required.
2. Generate the key at <https://api-dashboard.search.brave.com/app/keys>; it is shown once.
3. Cost, as of the February 2026 move to metered billing: $5 of monthly credit included, search at roughly $5 per 1,000 requests — about 1,000 queries per month inside the credit. Unused credit does not roll over, and requests are rejected once it is spent unless auto-reload is enabled. **Not re-checked on the vendor page** — `brave.com`, `api.search.brave.com` and `api-dashboard.search.brave.com` were all unreachable from the machine used for this repository on 2026-10-26 (connection timeouts), so confirm the current price before subscribing.
4. That same network finding is a precondition for the demo: subscribing, generating the key and calling the API all need those hosts to resolve and connect.
5. Keep the value in `BRAVE_API_KEY` in the Basalt process environment; `mcp_servers.yaml` holds `${BRAVE_API_KEY}` only. The server also accepts `BRAVE_API_KEY_FILE`, but Basalt's environment gate reads the variable.
6. Verify: `curl -s -o /dev/null -w '%{http_code}\n' -H "X-Subscription-Token: $BRAVE_API_KEY" "https://api.search.brave.com/res/v1/web/search?q=test"` returns `200`.

## What is verified, and what is not

Two kinds of claims appear in this directory, and they are not interchangeable:

| Claim | Meaning | Where it is checked |
| --- | --- | --- |
| Fixture replay | The directory loads, its declared tools resolve, and the script produces the expected business result from fixed input | `make example-tests`, and the release end-to-end suite |
| Real external run | The same flow against the real service (GitHub, DeepSeek Search, Brave, Fetch, Playwright, DBHub, a real model, a real notification receiver) | Not part of this repository's automated acceptance. DBHub 1.5.0 was exercised by hand on Windows (tool discovery plus a write rejected with `READONLY_VIOLATION`); GitHub, DeepSeek Search, Brave, Fetch, Playwright, real models and real notification receivers stay **unverified** — those credentials were not provided |

"Connects successfully" is neither of the above. A successful connection only proves that the server started and answered `tools/list`.

## Shared assets

| Path | What it is |
| --- | --- |
| [shared/mcp/docs-readonly/](shared/mcp/docs-readonly/) | Minimal read-only MCP server: `list_documents`, `read_document`, `document_metadata`. Standard library only, a `main` package in this module (never imported by the product binary), no write tools at all. `basalt demo install` exports the source into the workspace with a generated `go.mod` so it builds on its own |
| [shared/fixtures/docs/](shared/fixtures/docs/) | Fictional handbook pages used as the read-only root by `docs-diff` and `onboarding-buddy` |
| [shared/scripts/make_sample_db.py](shared/scripts/make_sample_db.py) | Builds the SQLite sample database from a schema file and a rows file; the `.db` itself is generated, never committed |

Both the fixtures and the scripts are fictional and contain no real names, accounts or credentials. `make_sample_db.py` refuses to overwrite an existing database unless you pass `--force`, which deletes and rebuilds it from the given schema and rows.

## External access

- The demo scripts never call the network themselves. They read JSON that the agent saved from MCP results, compute the numbers, and write JSON back. That keeps every reported figure re-computable.
- The MCP servers you install make their own outbound calls. What they reach is their own policy, not Basalt's: a GitHub server talks to GitHub, a browser server can reach whatever it is pointed at.
- `docs-readonly` reads only files under the root you give it, refuses absolute paths, parent references and reparse points, and has no write operations.

## Checks

```sh
make example-tests   # bundled read-only MCP server tests + every demo script's unit tests
make e2e             # release end-to-end scenarios against the candidate binary
```

`make example-tests` needs Python 3; a missing interpreter is a failure, not a skip.
