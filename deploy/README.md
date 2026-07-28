# deploy

Deployment assets land here in later milestones:

- **M2:** read-only RBAC ServiceAccount + Role for mcp-k8s, and the
  docker-compose stack (Jaeger) for local tracing.
- **Post-M4:** manifests for running bloodhound itself in-cluster;
  Helm chart is a stretch goal.

Nothing in v1 writes to a cluster — every manifest here is read-only access.
