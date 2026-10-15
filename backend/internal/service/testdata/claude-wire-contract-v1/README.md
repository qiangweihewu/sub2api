# Claude wire contract v1

All data in this directory is synthetic. `modeled_version` identifies an authored
fixture shape, not a captured Claude Code release. No fixture proves current
client behavior or upstream account enforcement.

`manifest.json` pins schema version 1 and lists independently authored request
and expected wire bodies. Tests execute `Forward` with explicit transparent or
protected policy settings. Complete and truncated SSE fixtures are replayed
with diagnostics off and on; bytes, usage and error outcomes must remain equal.

The mapped request and tool response fixtures additionally exercise the real
tool-name transforms, including their existing last-tool cache breakpoint.
Tool names change only at protocol name/reference fields; tool arguments,
Unicode, date punctuation and ordinary prose must stay unchanged.

To update: add synthetic inputs and hand-check expected outputs, record the
provenance, and run `go test ./internal/service -run '^TestClaudeWireContract'`.
Never generate expected output with the transform under test. Keep credentials,
customer content, account IDs and captured traffic out of this corpus. Any
future real-client observation requires separate explicit authorization and
sanitization; do not relabel these fixtures as observed.
