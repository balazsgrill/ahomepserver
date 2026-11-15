---
name: system-diagnostics
description: Steps for diagnosing device connectivity, sensor anomalies, and service health.
---

When investigating a device or service issue:
1. Query the current status via the `home_control` or `metrics_reader` MCP tools if reachable.
2. Check whether the issue is transient or recurring.
3. Summarize findings with:
   - **Status**: Healthy / Degraded / Critical
   - **Evidence**: Tool outputs and payload metrics
   - **Next Steps**: Concrete remediation steps

