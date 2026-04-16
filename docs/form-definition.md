# FormDefinition: declarative UI overlays for schema-driven forms

cozytempl generates create/edit forms by walking each
`ApplicationDefinition`'s `openAPISchema`. That keeps the UI
in sync with whatever Cozystack ships, but the tradeoff is
that the labels, hints, placeholders, and field order are
whatever the upstream schema says — which is not always what
an operator wants users to see.

`FormDefinition.cozytempl.cozystack.io/v1alpha1` is a
cluster-scoped CRD that overlays UI metadata on top of the
schema-generated form, without changing the downstream CRD or
the validation the API server applies.

## What it can override

Per field path:

- `label` — replaces the schema's `title`
- `hint` — replaces the schema's `description`
- `placeholder` — sets the HTML `placeholder` attribute on the
  rendered input
- `order` — render-order number within the field's group
  (lower first); unordered fields fall back to alphabetical
- `hidden` — drops the field from the rendered form. Existing
  values survive an edit save because the raw spec snapshot is
  merged through the YAML path; the UI just stops exposing the
  knob.

What it explicitly does **not** do in v1alpha1:

- Widget type overrides (forcing a text input into a dropdown,
  etc.)
- Conditional visibility (`show-when = other-field == X`)
- Default-value injection
- Enum labelling
- Validation overrides

Those may land in a future `v1beta1` once real operator
feedback drives concrete needs. The `v1alpha1` surface is
deliberately small so it can stabilise without accumulating
half-used knobs.

## Example

```yaml
apiVersion: cozytempl.cozystack.io/v1alpha1
kind: FormDefinition
metadata:
  # Name determines merge precedence: when multiple
  # FormDefinitions target the same kind they are folded in
  # name order, with a last-write-wins rule on path conflicts.
  # Prefix base definitions with "00-" and overlays with
  # "50-tenant-a-" etc. to keep the precedence operator-visible.
  name: postgres-production
spec:
  # Exact match against the ApplicationDefinition kind.
  # Case-sensitive; "Postgres" does not match "postgres".
  kind: Postgres
  fields:
    - path: replicas
      label: Replica count
      hint: Number of running Postgres pods. One is fine for dev, three is the production minimum.
      placeholder: "3"
      order: 0
    - path: storage.size
      label: Storage size
      hint: Total persistent volume for each replica.
      placeholder: "20Gi"
      order: 1
    - path: backup.enabled
      label: Backups
      hint: When on, a daily snapshot is uploaded to the configured S3 bucket.
    - path: internal.debug
      # Hidden from the form but still reachable via the YAML
      # tab for operators who need to flip it.
      hidden: true
```

## How the merge works

At render time the handler fetches every `FormDefinition`
visible to the caller and folds them into a single
`path → override` map. Every field path in the
schema-driven walker is then checked against that map:

1. If `hidden` is set, the field is dropped entirely.
2. Otherwise the schema-derived label / description /
   placeholder are replaced when the override has a non-empty
   value for them.
3. Within each render group (top-level scalars or a nested
   block like `backup.*`), fields with an explicit `order`
   render first in ascending order; fields without one follow
   in the schema's alphabetical order.

If no `FormDefinition` targets a kind, the renderer returns
the exact same output it did before the CRD existed — the
feature is strictly additive for existing clusters.

## Installing the CRD

The chart installs the CRD once at `helm install` time via
the Helm 3 `crds/` directory convention. `helm upgrade` does
not touch CRDs — that is intentional, because the CRD is a
cluster-wide contract shared across every `cozytempl` release.
Operators who want to manage the CRD lifecycle out-of-band
can remove `deploy/helm/cozytempl/crds/formdefinition.yaml`
before packaging the chart and apply the CRD manually.

## RBAC

Every cozytempl user fetches `FormDefinition` objects through
their own Kubernetes identity — same impersonated-dynamic-
client stack the schema service uses for
`ApplicationDefinitions`. The chart ships a `ClusterRoleBinding`
that binds `get/list/watch` on `formdefinitions.cozytempl.cozystack.io`
to `system:authenticated`; set
`formDefinition.readerBinding.create=false` or override the
`subjects` list if a stricter audience is required. Without
the binding, FormDefinitions silently do not apply and the
form falls back to schema-only rendering.

## Who can write a FormDefinition

Cluster-admin by default. The CRD has no namespace, so
applying one is a cluster-wide change. Consider:

- A `cozytempl-ui-editor` `ClusterRole` with `create/update`
  on `formdefinitions`, bound to a specific platform-eng group.
- A validating admission policy that rejects `FormDefinitions`
  whose `spec.kind` does not match any installed
  `ApplicationDefinition` — catches typos early.

Neither is shipped by the chart because they depend on the
operator's own RBAC topology.
