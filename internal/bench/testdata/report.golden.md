## Benchmark: trial

Benchmark `b1` (2 known defects, `01234567`). Target: o/r `6d36cd0...e619281`, replayed as draft #11. 2 run(s) per arm requested. [Workflow run](https://github.com/o/r/actions/runs/42/attempts/1).

Judge: `claude-x` anthropic at high effort. **Its verdicts are drafts**: a person confirms or overrides them (docs/benchmarking.md).

> **Missing runs:** glm has 1 of 2. A run that failed before recording telemetry, or whose job was canceled, has no row.

### Arms

| Arm | Ref | Group | Find model | Effort | Prompt | Tools | Commit | Runs |
|---|---|---|---|---|---|---|---|---|
| deepseek | `main` | `g1` | deepseek | medium | `aaaabbbb` | `11112222` | `1234567` | 2 |
| glm | `exp/glm` | `g2` | glm-5.3 | medium | `aaaabbbb` | `11112222` | `abcdef1` | 1 |

### Runs

| Run | Wall | Turns | s/turn | Tools/turn | Tokens in / out / reasoning / cached | Ended | Findings (blocking) | Recall | Extra real / false |
|---|---|---|---|---|---|---|---|---|---|
| deepseek 1 | 20m00s | 20 | 60 | 2.0 | 5000 / 700 / 0 / 0 | complete | 2 (0) | 1 of 2 | 1 / 0 |
| deepseek 2 | 30m00s | 30 | 60 | 1.0 | 0 / 0 / 0 / 0 | the pass deadline passed | 0 (0) | 0 of 2 | 0 / 0 |
| glm 1 | 10m00s | 10 | 60 | 2.5 | 1000 / 200 / 50 / 400 | complete | 1 (1) | 0 of 2 | 1 / 0 |

### By arm

Averages within each identity group; groups side by side.

| Arm | Group | Runs | Wall | Turns | s/turn | Tools/turn | Tokens in / out | Finished | Findings | Recall | Extra real / false |
|---|---|---|---|---|---|---|---|---|---|---|---|
| deepseek | `g1` | 2 | 25m00s | 25.0 | 60 | 1.4 | 2500 / 350 | 1 of 2 | 1.0 | 25% (0.50 of 2) | 0.5 / 0.0 |
| glm | `g2` | 1 | 10m00s | 10.0 | 60 | 2.5 | 1000 / 200 | 1 of 1 | 1.0 | 0% (0.00 of 2) | 1.0 / 0.0 |

### Known defects

✓ found, ½ partial, · missed; an override is marked with *.

| Defect | Source | Description | deepseek 1 | deepseek 2 | glm 1 |
|---|---|---|---|---|---|
| d1 | copilot | one | ✓ | · | · |
| d2 | buggrit | two | ·* | · | · |

<details><summary>Matches and the judge's reasoning</summary>

- **deepseek 1, d1** (found): finding 1, a.go:2 first. same bug
- **deepseek 1, d2** (missed, overridden from partial): finding 1, a.go:2 first. close Override: different file

</details>

### Extra findings

- **deepseek 1**, finding 2: P3 extra one at q.go:3-5: **real** (draft). plausible
- **glm 1**, finding 1: P1 a \| pipe at z.go:1: **real** (overridden from false). misread Override: it's real

### Findings by run

<details><summary>deepseek 1: 2 finding(s), run <code>r-ds-1</code></summary>

1. **P2** first, at `a.go:2`
   it breaks
2. **P3** extra one, at `q.go:3-5` (class: c, 1 instance(s))

</details>
<details><summary>deepseek 2: 0 finding(s), run <code>r-ds-2</code></summary>

No findings.

</details>
<details><summary>glm 1: 1 finding(s), run <code>r-glm</code></summary>

1. **P1** a \| pipe, at `z.go:1`
   wrong

</details>
