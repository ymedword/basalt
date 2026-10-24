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

The development URL includes `/basalt/`. Use `dev --host 127.0.0.1` for a loopback-only preview. The built artifact is in `.vitepress/dist`; `npm run preview` serves it through VitePress's static server. VitePress 1.6.4's static preview ignores the host option and binds all interfaces, so it does not provide the same loopback binding. Neither command publishes to Pages. The separate Pages workflow is manual and must be triggered by an authorized maintainer. CI builds an artifact and never deploys it.

## Content and commands

English pages live at the directory root; Chinese pages live under `zh/`. Maintain the four matching pages together. Navigation, footer destinations and language mappings come from `.vitepress/data/navigation.mjs`. Language switching preserves the current page, query and fragment; unknown pages return to the selected language's homepage.

`.vitepress/data/quickstart.mjs` is the command source. The two root READMEs contain exactly one `quickstart-commands:start` / `quickstart-commands:end` marker pair. Update the source, both marked command sequences and both quickstart pages together. `content.test.mjs` extracts and compares all six commands; it also verifies prerequisites and lifecycle instructions. Do not add an independent command list to a Vue component. Keep `DURATION_MEASURED=false` until a complete tutorial has been timed; do not add estimated durations.

`.vitepress/data/capabilities.mjs` separates six platform highlights from five implementation capabilities. Each item references existing implementation evidence. Do not present architecture goals as delivered features.

The root READMEs introduce the product, explain its benefits, show one compact architecture image, and provide a runnable quickstart and task-oriented documentation links. Keep detailed architecture views in the architecture pages. Windows build instructions and the complete minimal configuration remain available in collapsible sections; the six canonical commands appear in three blocks inside the single marker pair.

## Re-capture the local CLI demo

The homepage explicitly identifies its scripted local model. `demo/transcript.json` contains actual CLI stdout, including the tool progress line and final reply, rather than an invented transcript. Its command comes from `QUICKSTART_COMMANDS`; the final output lines must reconstruct the stub's complete response. The stub performs no external inference and uses a non-secret placeholder key.

The terminal presentation uses English progress labels in both page languages. `data/demo.mjs` translates the known tool-progress label for display; the original capture, model response and product CLI/logs are unchanged. Tests compare the raw model response and the translated presentation separately.

1. Build the existing Basalt binary using the root quickstart instructions. Start `npm run demo` in another terminal. It binds only `127.0.0.1:18769`.
2. Create an isolated, disposable workspace outside your real data. Use the quickstart's minimal configuration, replacing the provider endpoint with `http://127.0.0.1:18769/v1`, the default model with `demo-model`, and the management address with an unused loopback port. Export the two required environment values as local placeholders. Never reuse real credentials.
3. Follow the public quickstart: initialize, create `demo`, list it, start the server, and use the management console to select the model connection, set model `demo-model`, declare the `read_file` tool, save and enable the Agent. Copy the repository's public README.md into the isolated Agent's own directory, next to its AGENT.md. File-tool paths are relative to that directory. New Agents are disabled; registration alone does not enable them. Stop the server before running CLI chat because the workspace lock is exclusive.
4. Capture stdout from the exact chat command exported by `QUICKSTART_COMMANDS[4]`; retain stderr separately and record the real exit code. Query sessions afterwards. This scripted greeting invokes `read_file` on the sample README.md and then prints a project summary; a missing or denied file returns an explicit failure response. Re-capture if the binary, command or stub response changes.
5. Set `capturedAt` to the actual UTC capture time, `source` to `local-openai-compatible`, `stub` to `website/demo/local-model.mjs`, `command` to the command source, `lines` to the non-empty stdout lines, and `exitCode` to the actual result. The final output lines must match the complete multiline stub response. Do not edit output text to make a check pass. Stop the owned stub and server after capture.

The checked-in sample was captured against source commit `eb97cd539e51e2b3db15afcfcc1dfa295eaebe77`. Runtime smoke checks used the management API to configure and enable the isolated Agent; browser clicking remains a separate review requirement. A local stub result does not prove a real vendor model or deployment works.

## Diagram sources and exports

`diagrams/basalt-diagrams.drawio` contains twelve editable, uncompressed mxGraph pages: the project overview plus five focused views, each in English and Chinese. The matching static SVGs are under `public/diagrams/en/` and `public/diagrams/zh/`. All twelve currently describe source commit `eb97cd539e51e2b3db15afcfcc1dfa295eaebe77`.

The README uses only `system-architecture.svg`, a compact five-layer summary with cross-cutting security and operations. `project-overview.svg` expands the same access, application, execution engine, infrastructure service and storage layers with external systems and concrete components. Scheduling belongs to the application layer. These are logical responsibility layers, not separate deployment units or a guarantee of process isolation.

The initial drawio and SVG files were generated from the same explicit node/edge model. A draw.io executable was unavailable during implementation; no draw.io binary export is claimed. For future edits, open the source in diagrams.net, select each named page, and export its drawing as SVG with text converted to normal SVG text, no embedded HTML or external images, and a stable page size. Export English and Chinese with identical `viewBox` values. Keep the fixed ID, actual source commit, title and description metadata on each output. Inspect the result at full size before replacing the checked-in SVGs.

If using the draw.io desktop CLI, export a selected page with the following command and repeat for each page index and locale. Add the required metadata after export; inspect that the exporter has not introduced `foreignObject`.

```bash
drawio --export --format svg --page-index 0 --output public/diagrams/en/project-overview.svg diagrams/basalt-diagrams.drawio
```

`check-diagrams.mjs` checks all six IDs, both locales, provenance against real Git history, metadata, page count, viewBox parity, external references and execution-flow failure branches. The original five views remain required alongside the overview. A full Git history is required (`fetch-depth: 0` in both workflows). Never replace source commits with arbitrary strings or the timestamp of export.

The website documentation page explains the Agent OS concept, independent/shared responsibilities, current capabilities and a separately labelled planned direction. Maintain this distinction in both languages. The overview depicts current code; future distributed execution or enterprise identity must not be added to it as existing components.

## Checks and review

The Node suite covers route mappings, command/content parity, negative fixtures, local model protocol, reduced-motion data, color contrast and workflow boundaries. `npm run check` scans built links and diagram assets. `node scripts/check-scope.mjs` is a separate working-tree check; it is intentionally not in the website build script.

Before review, inspect desktop, tablet and phone layouts, keyboard focus, same-page language switching, animation pause/replay, reduced motion, 404 fallback and offline operation in a browser. Static assertions do not substitute for those checks. Keep public pages and repository links within the approved public documentation set; internal plans, local work records and credentials are never site content. External links are confined to the approved repository and the explicitly documented loopback console.

Do not claim publication, real-model verification or independent acceptance from a local build. Preserve the separation between implementation self-checks and review results.
