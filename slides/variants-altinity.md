---
marp: true
theme: default
paginate: true
backgroundColor: #ffffff
color: #1d1d1f
style: |
  section {
    font-family: 'SF Pro Display', 'Helvetica Neue', sans-serif;
    font-size: 28px;
  }
  h1, h2 {
    color: #0071e3;
  }
  code {
    background: #f5f5f7;
    color: #d63384;
    font-size: 22px;
  }
  pre {
    background: #f5f5f7 !important;
    border-radius: 8px;
    font-size: 20px;
    color: #1d1d1f;
  }
  table {
    font-size: 24px;
  }
  th {
    background: #0071e3;
    color: #fff;
  }
  td {
    background: #f5f5f7;
  }
  strong {
    color: #e3002b;
  }
  a {
    color: #0071e3;
  }
  section.lead h1 {
    font-size: 52px;
    text-align: center;
  }
  section.lead p {
    text-align: center;
    font-size: 24px;
    color: #86868b;
  }
---

<!-- _class: lead -->

# Variant A
## "Isolation at every layer"

---

## Fully Managed on Altinity.Cloud

Isolator sandboxes your **agent**. Altinity.Cloud sandboxes your **data**.

| Layer | What's isolated | How |
|-------|----------------|-----|
| AI agent | Filesystem, network, credentials | Isolator (macOS users + pf) |
| ClickHouse | Production data, queries, storage | Altinity.Cloud (fully managed) |

Your agent can `clickhouse-client` into a dev cluster — without touching production.

- **Dedicated environments** per team, project, or experiment
- **SOC 2, HIPAA** — compliance built in, not bolted on
- **Zero ops** — upgrades, backups, scaling handled for you

The same principle: **full autonomy within a controlled boundary**.

[altinity.cloud](https://altinity.cloud)

---

<!-- _class: lead -->

# Variant B
## "From laptop to cloud"

---

## Fully Managed on Altinity.Cloud

You've seen isolation on a laptop. Now scale it.

**Local development** (Isolator):
```bash
iso acm claude                    # sandboxed agent
docker-compose up clickhouse      # local ClickHouse in Docker
```

**Production** (Altinity.Cloud):
```bash
clickhouse-client --host acm-dev.altinity.cloud --secure
# Managed ClickHouse — same queries, zero ops
```

Altinity.Cloud gives every team their own ClickHouse:
- **Isolated clusters** — dev, staging, production
- **RBAC + network policies** — agents get read-only access to dev, nothing else
- **Automatic backups, upgrades, monitoring** — you focus on the product

**Isolator for the agent. Altinity.Cloud for the data.**

[altinity.cloud](https://altinity.cloud)

---

<!-- _class: lead -->

# Variant C
## "The missing piece"

---

## Fully Managed on Altinity.Cloud

Isolator controls **what the agent can reach**.
But who controls **what the agent can query**?

| Risk | Without Altinity.Cloud | With Altinity.Cloud |
|------|----------------------|-------------------|
| Agent drops a table | 💥 Production down | ✅ RBAC: read-only for agents |
| Agent scans all data | 💥 Compliance violation | ✅ Row-level security |
| Cluster runs out of memory | 💥 Manual recovery | ✅ Auto-scaling, quotas |
| No backups before migration | 💥 Data loss | ✅ Automated backups |

**Sandbox the agent. Sandbox the database. Sleep well.**

- Fully managed ClickHouse — SOC 2, HIPAA, zero ops
- Per-environment isolation — dev / staging / prod
- Native MCP integration — agents query ClickHouse directly

[altinity.cloud](https://altinity.cloud)
