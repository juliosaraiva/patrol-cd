# Lua health check and resource actions for a CRD

`ops.example.com/BackupSchedule` (CRD in `crd/`) stands for any operator-managed resource
whose health Patrol CD cannot infer.

```
ops.example.com/BackupSchedule/
  health.lua              health assessment (Healthy, Progressing, Degraded, Suspended)
  health_test.yaml        expected health per fixture in testdata/
  actions/discovery.lua   which actions are offered (and enabled) for an object
  actions/<name>/action.lua
  actions/action_test.yaml  discovery and action cases, fixtures in actions/testdata/
```

The layout and test file format are those of the built-in customizations in
`internal/lua/customizations`, so a script can later be contributed as a built-in.

Patrol CD reads customizations from `patrol-cm`; the scripts are embedded in
`examples/platform/settings/patrol-cm.yaml` under
`resource.customizations.health.ops.example.com_BackupSchedule` and
`resource.customizations.actions.ops.example.com_BackupSchedule`. `test/examples` runs every
case of the test files against the scripts as parsed from that ConfigMap and fails if the
embedded copies differ from the files here.

Action parameters (`set-retention` takes `days`) reach the script as strings in
`actionParams`. Running an action needs the RBAC permission
`applications, action/ops.example.com/BackupSchedule/<action>`.
