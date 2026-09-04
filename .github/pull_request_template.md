### Title
<!-- Follow Conventional Commits: <type>[optional scope]: <description> -->
<!--
Examples:
feat(casserver): support V2 multi-range reconstruction
fix(shardformat): handle files with no verification entries
docs: add SigV4 signer walkthrough to PROTOCOL.md

Types: feat, fix, docs, test, refactor, perf, style, build, ci, chore, revert
-->

---

### Description
<!-- What changed, and why? Link to any relevant xet-core source or issue this addresses. -->

---

### Type of change
- [ ] New feature
- [ ] Bug fix
- [ ] Protocol/wire-compatibility fix (see [docs/PROTOCOL.md](docs/PROTOCOL.md))
- [ ] Documentation only
- [ ] Refactor / internal cleanup

---

### Testing
<!-- What did you run, and what passed? -->
- [ ] `make test` (unit tests)
- [ ] `make integration-test` (bash integration suite)
- [ ] `make integration-test` after `make install` (real `hf` CLI round-trip via `hf_cli_roundtrip.sh`)
- [ ] If this touches wire format/protocol behavior: verified against a real captured client payload (see `internal/*/testdata/`), not just synthetic fixtures

<!-- If you captured new real-client bytes to add as a regression fixture, say so here and where they came from. -->

---

### Review checklist
- [ ] PR title follows Conventional Commits
- [ ] New/changed exported types and functions have doc comments
- [ ] New packages have a `// Package foo ...` comment (see [CONTRIBUTING.md](CONTRIBUTING.md#3-working-with-documentation))
- [ ] `docs/PROTOCOL.md` updated if this changes or discovers a wire-format quirk
- [ ] `CHANGELOG.md` updated under `[Unreleased]`
