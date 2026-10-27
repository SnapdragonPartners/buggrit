# Architecture decision records

A decision that changes how buggrit is built, released or trusted, and that
a reader of [design.md](../design.md) would otherwise have to reconstruct,
gets a record here: its context, the decision, its consequences, and the
alternatives considered. [design.md](../design.md) stays the binding design
and links the record; §13 there lists the smaller decisions.

Records are numbered in order (`NNNN-short-title.md`) and not rewritten once
accepted. A later record that changes one says so, and the earlier one's
status points to it.

| ADR | Decision |
|---|---|
| [0001](0001-immutable-release-references.md) | A release pins its own actions, and release tags are protected |
| [0002](0002-the-releaser-moves-v0.md) | The releaser moves `v0`, not a workflow |
