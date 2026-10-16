// Entry point evaluated by the directory source. The top-level function receives the
// Application's TLAs; std.extVar reads its extVars.
local kube = import 'kube-lite/kube.libsonnet';  // resolved through spec.source.directory.jsonnet.libs
local params = import 'params.libsonnet';  // relative to this file

function(imageTag, features={})
  local env = std.extVar('environment');
  local replicas = std.extVar('replicas');
  local opts = params.defaults + features;
  local labels = { 'app.kubernetes.io/name': params.name, 'app.kubernetes.io/part-of': 'patrol-examples', environment: env };

  [
    kube.configMap(params.name + '-config', labels, {
      PODINFO_UI_MESSAGE: 'Hello from Jsonnet (%s)' % env,
      PODINFO_UI_COLOR: params.colors[env],
    }),
    kube.deployment(params.name, labels, replicas, '%s:%s' % [params.image, imageTag], params.port, params.name + '-config'),
    kube.service(params.name, labels, params.port),
  ] + (if opts.networkPolicy then [kube.allowIngressPolicy(params.name, labels, params.port)] else [])
