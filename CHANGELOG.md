# Changelog

## 0.1.0

OpenCluster 0.1 establishes the first supported control-plane contract.

- Published the authenticated `/api/v1` product API and separate `/webhooks/v1` intake API from one canonical OpenAPI document.
- Added durable Integrations, Alert Events, Incidents, Investigations, Conversations, Postmortems, webhook delivery replay, and the Generic Webhook Integration Type.
- Added released Relay protocol consumption, enrolment compatibility checks, fleet visibility, and separate same-origin frontend and control-plane containers.
- Added Anthropic and Z.AI investigation adapters with bounded tools, cited structured results, spend limits, and release evaluations.

This is a clean-break pre-1.0 release. The schema installs from one clean baseline; existing
development databases must be recreated because no in-place upgrade path is provided.
