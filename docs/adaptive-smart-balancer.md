# Adaptive Smart Balancer

`adaptiveSmart` is a panel-managed balancer strategy that keeps cached health data for each selected outbound, shapes the runtime selector set that Xray receives, and shows a live health report in the Balancers tab.

How it differs from `random`, `roundRobin`, `leastPing`, and `leastLoad`:

- It scores each member from `0` to `100` using cached checks, latency, recent success rate, timeouts, and repeated failures.
- It can quarantine weak members for a short period and bring them back with low recovery weight.
- It shows per-member status, score, weight, latency, and quarantine time in the panel.

Why cached health checks are used:

- The panel can run on small VPS servers.
- Cached checks avoid per-request probes, large buffers, and noisy logs.
- Throughput probe is disabled by default because it generates extra test traffic.

Current runtime behavior:

- The panel stores `adaptiveSmart` settings separately and keeps health scoring active.
- At runtime, the panel feeds Xray a native fallback strategy such as `leastLoad`, `roundRobin`, or `random`, but it narrows that balancer's selector set to healthy, recovering, or unknown members.
- This keeps the runtime config valid while letting adaptive health directly influence which outbounds stay eligible for live traffic.

Health Report meanings:

- `healthy`: member score is above the minimum threshold.
- `recovering`: member left quarantine and is returning with a low initial weight.
- `quarantined`: member is temporarily removed from selection.
- `unhealthy`: member is below the minimum score.
- `unknown`: no cached health data exists yet.
