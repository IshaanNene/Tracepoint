### TracePoint: checkout

**Validity:** valid · **SLO:** **fail** · **Bottleneck:** db · **Confidence:** medium

The database tier is the most likely source of the slowdown\. In the one incident that reached users, covering 6s, application latency rose together with the db probe, which went hot first, by 1 bucket\. No server\-side telemetry corroborated it\.

<details><summary>Evidence</summary>

- `incident` inc\-1 \(correlated\) from 20s to 26s\: http p99 900ms, db p99 400ms; db led by 1 bucket
- `correlation` over the whole run, application p99 and db p99 had Spearman rho 0\.94 at lag \+0 across 60 buckets
- `sample_size` http 6000 ops, db 6000 ops, redis 6000 ops; 100% of measured buckets held enough samples

</details>

**Next steps**

1. look at the database during 20s\-26s\: lock waits, long transactions, slow\-query log and pg\_stat\_activity or the processlist
2. enable telemetry for the database, so lock waits and pool saturation are sampled on the same clock
3. check the application's database pool size against its concurrency

> Shared\-clock correlation shows association, not causation\.

#### SLOs

| Runner | Metric | Budget | Actual | Result |
| --- | --- | ---: | ---: | --- |
| http | p99 | 250ms | 900ms | **fail** |
| http | error\_rate | 1.0% | 0% | pass |

#### Runners

| Runner | Operations | Errors | Achieved/s | p50 | p95 | p99 | p99 service |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| http | 6000 | 0% | 100 | 10ms | 18ms | 20ms | 20ms |
| db | 6000 | 0% | 100 | 1.0ms | 1.8ms | 2.0ms | 2.0ms |
| redis | 6000 | 0% | 100 | 0.25ms | 0.45ms | 0.50ms | 0.50ms |

#### Incidents

| Incident | Class | Window | Culprit | App peak p99 |
| --- | --- | --- | --- | ---: |
| inc\-1 | correlated | 20s–26s | db | 900ms |

#### Findings

- **warn** `SAME_HOST` the generator and a target share a host — run the generator elsewhere

<sub>run `20260924T100000Z-abc123` · 60s · seed 7 · TracePoint test · runs/20260924T100000Z\-abc123</sub>
