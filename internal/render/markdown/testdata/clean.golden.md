### TracePoint: checkout

**Validity:** valid · **SLO:** pass · **Bottleneck:** none · **Confidence:** high

Nothing stood out\: the run stayed within its budgets, the generator kept to its schedule, and no tier's latency departed from its own baseline\.

<details><summary>Evidence</summary>

- `sample_size` 3000 operations at 100/s, p99 20\.0ms, 0\.00% errors

</details>

**Next steps**

1. configure slo budgets so the run has something to pass or fail against
2. raise the rate, or run a ramp, to find where latency starts to degrade

> Shared\-clock correlation shows association, not causation\.

#### Runners

| Runner | Operations | Errors | Achieved/s | p50 | p95 | p99 | p99 service |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| http | 3000 | 0% | 100 | 10ms | 18ms | 20ms | 20ms |
| db | 3000 | 0% | 100 | 1.0ms | 1.8ms | 2.0ms | 2.0ms |
| redis | 3000 | 0% | 100 | 0.25ms | 0.45ms | 0.50ms | 0.50ms |

No incidents.

<sub>run `20260924T100000Z-abc123` · 30s · seed 7 · TracePoint test</sub>
