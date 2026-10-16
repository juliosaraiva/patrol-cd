local k = import 'k.libsonnet';
local params = import 'params.libsonnet';
function(ns='default', replicas=1)
  k.cm('obj') + {
    metadata+: { namespace: ns },
    data: {
      replicas: std.toString(replicas),
      code: std.toString(std.extVar('code').a),
      color: params.color,
    },
  }
