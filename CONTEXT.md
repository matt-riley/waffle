# Waffle Operations

This context covers how an operator establishes and maintains a working Waffle installation.

## Language

**First Deployment**:
The transition from no Waffle installation to a healthy installation ready for use.
_Avoid_: Setup, bootstrap

**Managed Setup**:
The default deployment mode in which Waffle owns internal service accounts and credentials while the operator supplies only external credentials required by chosen integrations.
_Avoid_: Zero-config, automatic setup

**Internal Credential**:
A secret identity used only by components within one Waffle installation and not issued by an external service.
_Avoid_: Deployment secret, generated secret

**External Credential**:
A secret issued by an external service that grants Waffle access to an operator-selected integration.
_Avoid_: User secret, supplied secret

**Operator Override**:
An explicit replacement for one Managed Setup decision that leaves all unrelated decisions under Waffle's ownership.
_Avoid_: Advanced mode, expert mode

**Deploy Intent**:
An operator's request for Waffle to create or update a working installation without requiring infrastructure design decisions.
_Avoid_: Deployment profile, topology selection

**Installed**:
A Waffle installation whose binary, service definition, state paths, and management commands are present but which has no validated default model.
_Avoid_: Deployed, partially ready

**Ready**:
An **Installed** Waffle installation with a validated default model whose service has passed its health checks.
_Avoid_: Installed, healthy enough

**Provider Connection**:
A named configuration that gives Waffle access to one model-provider API or compatible endpoint.
_Avoid_: Provider, backend

**Model Alias**:
A stable local name that selects one upstream model through exactly one **Provider Connection**.
_Avoid_: Model name, provider model

## Relationships

- A **First Deployment** uses **Managed Setup** by default.
- **Managed Setup** keeps internally owned operational details outside the operator's required decisions.
- **Managed Setup** generates and securely retains every **Internal Credential**.
- The operator supplies an **External Credential** only when enabling its associated integration.
- An **Operator Override** replaces exactly one managed decision without disabling **Managed Setup** elsewhere.
- A **Deploy Intent** starts **Managed Setup** using Waffle-owned defaults and any explicit **Operator Overrides**.
- A **First Deployment** produces an **Installed** Waffle installation without requiring a **Provider Connection**.
- An **Installed** Waffle installation becomes **Ready** after one **Model Alias** is validated and selected as the default.
- A **Provider Connection** may own one or more **Model Aliases**, and one Waffle installation may contain multiple **Provider Connections**.

## Example dialogue

> **Dev:** "Does the operator need to choose a database username and password during the **First Deployment**?"
> **Domain expert:** "No — **Managed Setup** owns those details unless the operator explicitly chooses to control them."
> **Dev:** "Which infrastructure profile should we ask them to choose?"
> **Domain expert:** "None — their **Deploy Intent** is enough unless they provide an **Operator Override**."
> **Dev:** "Must deployment wait for a model-provider key?"
> **Domain expert:** "No — deployment reaches **Installed**, then `waffle provider add` validates a **Provider Connection** and transitions the installation to **Ready**."

## Flagged ambiguities

- "Unless they want it to be" was resolved as independent **Operator Overrides**, not an all-or-nothing advanced mode.
- "Deploy" means expressing **Deploy Intent**, not selecting an infrastructure stack or topology.
