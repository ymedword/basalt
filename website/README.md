# Website maintenance

This directory contains Basalt's static public website. The Go runtime does not depend on this toolchain. Use Node **24.22.0** from `.nvmrc`; the sole direct build dependency is VitePress **1.6.4**. All illustrations, diagrams and fonts are local.

From this directory:

```bash
npm ci
npm test
npm run build
npm run check
npm run dev -- --host 127.0.0.1
```

The development URL includes `/basalt/`. Use `dev --host 127.0.0.1` for a loopback-only preview. The built artifact is in `.vitepress/dist`; `npm run preview` serves it through VitePress's static server. VitePress 1.6.4's static preview ignores the host option and binds all interfaces, so it does not provide the same loopback binding. Neither command publishes to Pages. The separate Pages workflow publishes automatically after the `CI` workflow succeeds on `main`; an authorized maintainer can still trigger it manually through `workflow_dispatch`. It checks out the CI run's head commit and refuses to publish when that commit is no longer the default branch tip. CI itself builds an artifact and never deploys it.

## Content and commands

English pages live at the directory root; Chinese pages live under `zh/`. Maintain the four matching pages together. Navigation, footer destinations and language mappings come from `.vitepress/data/navigation.mjs`. Language switching preserves the current page, query and fragment; unknown pages return to the selected language's homepage.

`.vitepress/data/quickstart.mjs` is the command source. The root README is English only and contains exactly one `quickstart-commands:start` / `quickstart-commands:end` marker pair. Update the source, the marked README command sequence and both language versions of the website quickstart together. `content.test.mjs` extracts and compares all six commands; it also verifies prerequisites and lifecycle instructions. Do not add an independent command list to a Vue component. Keep `DURATION_MEASURED=false` until a complete tutorial has been timed; do not add estimated durations.

`.vitepress/data/capabilities.mjs` separates six platform highlights from five implementation capabilities. Each item references existing implementation evidence. Do not present architecture goals as delivered features.

The root README introduces the product, explains its benefits, shows one compact architecture image, and provides a runnable quickstart and task-oriented documentation links. Its centered header reuses the fixed-color blue rock-column icon; the heading and surrounding text use GitHub's theme colors. It has no language selector; the linked website remains bilingual. Keep detailed architecture views in the architecture pages. Windows build instructions and the complete minimal configuration remain available in collapsible sections; the six canonical commands appear in three blocks inside the single marker pair.

## Licensing

The repository root holds the authoritative `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES`. `public/` contains byte-identical copies so the built site distributes the same license and third-party notices at its root; `content.test.mjs` checks the copies against the repository files. Update both locations together.

## Re-capture the local CLI demo

The homepage explicitly identifies its scripted local model. `demo/transcript.json` records one actual scheduled execution: the scheduler trigger log line as emitted on the log stream, followed by the reply lines exactly as recorded for that run, rather than an invented transcript. One transport fact matters when re-capturing: `basalt serve` writes every log to stderr and prints no assistant reply to stdout, so the trigger line comes from stderr and the reply lines come from the run's recorded messages. Its command is `DEMO_COMMAND` in `data/demo.mjs` (`basalt serve`, the process whose scheduler fires the task); the reply lines must reproduce the stub's complete scheduled response. The stub performs no external inference and uses a non-secret placeholder key.

The terminal presentation uses English labels in both page languages. `data/demo.mjs` translates the known `msg=调度触发` label for display; the original capture, model response and product CLI/logs are unchanged. Tests compare the raw model response and the translated presentation separately.

1. Build the existing Basalt binary using the root quickstart instructions. Start `npm run demo` in another terminal. It binds only `127.0.0.1:18769` and answers the scheduled message with a fixed reply.
2. Create an isolated, disposable workspace outside your real data. Use the quickstart's minimal configuration, replacing the provider endpoint with `http://127.0.0.1:18769/v1`, the default model with `demo-model`, and the management address with an unused loopback port. Export the two required environment values as local placeholders. Never reuse real credentials.
3. Initialize and create `demo`, then give its AGENT.md a `schedule` whose `message` equals `SCHEDULED_MESSAGE` from `demo/local-model.mjs` (cron `*/5 * * * * *`, any fixed zone) and no tools. Start the server, select the local model connection, save and enable the Agent. New Agents are disabled; registration alone does not enable them.
4. Start `basalt serve` with its log stream (stderr) redirected to a file and wait until that stream contains a `msg=调度触发` INFO line and the run reaches `completed`. Send SIGTERM for the documented graceful shutdown and record the real exit code (the checked-in capture observed `0`). Query `runs` and `messages` for that run ID and read the reply text from the recorded messages — `serve` itself prints no assistant reply to stdout. The capture is a scheduler trigger, not a manual chat; do not substitute a chat transcript.
5. Set `capturedAt` to the trigger line's UTC time, `source` to `local-openai-compatible`, `stub` to `website/demo/local-model.mjs`, `command` to `DEMO_COMMAND`, `lines` to the trigger log line followed by the reply lines from the recorded messages, and `exitCode` to the observed serve exit code. The reply lines must match the complete multiline stub response. Do not edit output text to make a check pass. Stop the owned stub and server after capture.

The checked-in sample was collected from a locally built binary of a working tree based on commit `2bfcede9db06baf7cec27f1adb825a8e1ffb3969` that also carried the then-uncommitted US-25C changes, which ship in v0.2.1. Runtime smoke checks used the management API to configure and enable the isolated Agent; browser clicking remains a separate review requirement. A local stub result does not prove a real vendor model or deployment works.

## Diagram sources and exports

`diagrams/basalt-diagrams.drawio` contains twelve editable, uncompressed mxGraph pages: the project overview plus five focused views, each in English and Chinese. The matching static SVGs are under `public/diagrams/en/` and `public/diagrams/zh/`. The system-architecture and deployment views carry the US-25 Compose multi-Worker topology and the US-25C published-asset distribution and pinned admission. Their provenance points to base commit `2bfcede9db06baf7cec27f1adb825a8e1ffb3969`; `data-working-tree-feature="US25C"` names the feature added beyond that base. This is a source-maintenance annotation, not a claim that the feature exists in the base commit. Drawio and SVG cells were updated together; no draw.io executable export is claimed.

The README uses only `system-architecture.svg`, a compact five-layer summary with cross-cutting security and operations. `project-overview.svg` expands the same access, application, execution engine, infrastructure service and storage layers with external systems and concrete components. Scheduling belongs to the application layer. These are logical responsibility layers, not separate deployment units or a guarantee of process isolation.

The initial drawio and SVG files were generated from the same explicit node/edge model. A draw.io executable was unavailable during implementation; no draw.io binary export is claimed. For future edits, open the source in diagrams.net, select each named page, and export its drawing as SVG with text converted to normal SVG text, no embedded HTML or external images, and a stable page size. Export English and Chinese with identical `viewBox` values. Keep the fixed ID, actual source commit, title and description metadata on each output. Inspect the result at full size before replacing the checked-in SVGs.

If using the draw.io desktop CLI, export a selected page with the following command and repeat for each page index and locale. Add the required metadata after export; inspect that the exporter has not introduced `foreignObject`.

```bash
drawio --export --format svg --page-index 0 --output public/diagrams/en/project-overview.svg diagrams/basalt-diagrams.drawio
```

`check-diagrams.mjs` checks all six IDs, both locales, provenance against real Git history, metadata, page count, viewBox parity, external references and execution-flow failure branches. The original five views remain required alongside the overview. A full Git history is required (`fetch-depth: 0` in both workflows). Never replace source commits with arbitrary strings or the timestamp of export.

The website documentation page explains the Agent OS concept, independent/shared responsibilities, current capabilities, engineering verification and a separately labelled planned direction. Maintain this distinction in both languages. The overview depicts current code; future Kubernetes, database HA or enterprise identity must not be added to it as existing components.

## Checks and review

The Node suite covers route mappings, command/content parity, negative fixtures, local model protocol, reduced-motion data, color contrast and workflow boundaries. `npm run check` scans built links and diagram assets. `node scripts/check-scope.mjs` is a separate working-tree check; it is intentionally not in the website build script.

Before review, inspect desktop, tablet and phone layouts, keyboard focus, same-page language switching, animation pause/replay, reduced motion, 404 fallback and offline operation in a browser. Static assertions do not substitute for those checks. Keep public pages and repository links within the approved public documentation set; internal plans, local work records and credentials are never site content. External links are confined to the approved repository and the explicitly documented console origins (loopback and the demo instance).

Do not claim publication, real-model verification or independent acceptance from a local build. Preserve the separation between implementation self-checks and review results.
