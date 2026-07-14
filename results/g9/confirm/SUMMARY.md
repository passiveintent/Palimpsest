## Confirmatory results (pooled over seeds {41,42,43}, locked kappa=0.7 c_gate=1.0)

### Detection rate by detector (post-fix), fraction of bursts

| scenario | bursts | CS (a) | subst (c) | storm | k30 | k15 | routed |
| --- | --- | --- | --- | --- | --- | --- | --- |
| s1 payment weak signal +1.0 | 9 | 0/9 | 0/9 | 0/9 | 0/9 | 0/9 | 0/9 |
| s2 AZ outage 30min +100 | 3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 |
| s4 sub-keyframe 20-30s +30 | 90 | 0/90 | 0/90 | 0/90 | 45/90 | 90/90 | 30/90 |
| s6 deploy wave 30% +15 | 3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 |

### S3 magnitude ramp — post-fix CS detection (K1 monotonicity)

| magnitude | CS post | CS pre | subst (c) post |
| --- | --- | --- | --- |
| 1x | 0/90 | 89/90 | 0/90 |
| 2x | 0/90 | 67/90 | 0/90 |
| 5x | 0/90 | 46/90 | 0/90 |
| 10x | 0/90 | 46/90 | 57/90 |
| 20x | 52/90 | 46/90 | 90/90 |

### Detection lag where detected (post-fix), seconds — CS is never fastest

| scenario | CS lag | subst (c) lag | storm lag |
| --- | --- | --- | --- |
| s2 | 210 | 30 | 0 |
| s6 | 197 | 30 | 0 |

### Quiet-month precision (s5, 2 days/seed)

| config | quiet events/day (per seed) | mean support |
| --- | --- | --- |
| prefix | [5415, 5441, 5415] | 165.2 |
| postfix | [1, 0, 0] | 4.6 |

### Byte ledger, post-fix (KiB/day, mean over seeds)

| scenario | Palimpsest total | of which sketch/RESIDUAL | Gorilla 1.37B | keyframe 30s | keyframe 15s | routed-exact 5% |
| --- | --- | --- | --- | --- | --- | --- |
| s1 | 28974.9 | 14610.9 | 10415.0 | 27343.5 | 53316.2 | 14895.7 |
| s2 | 28830.9 | 13614.7 | 10415.0 | 26947.8 | 53928.2 | 15125.7 |
| s4 | 28096.0 | 14606.0 | 10415.0 | 26780.0 | 53548.4 | 14021.8 |
| s6 | 29197.2 | 13731.4 | 10415.0 | 27868.4 | 53812.6 | 15465.9 |
| s5 | 27875.4 | 14610.9 | 10415.0 | 26527.3 | 53053.7 | 13796.2 |
