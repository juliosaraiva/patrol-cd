// kube-lite: a tiny helper library vendored into the repository (in a real repository
// this directory is typically managed by jsonnet-bundler under vendor/).
{
  configMap(name, labels, data):: {
    apiVersion: 'v1',
    kind: 'ConfigMap',
    metadata: { name: name, labels: labels },
    data: data,
  },

  deployment(name, labels, replicas, image, port, configMapName):: {
    local selector = { 'app.kubernetes.io/name': labels['app.kubernetes.io/name'] },
    apiVersion: 'apps/v1',
    kind: 'Deployment',
    metadata: { name: name, labels: labels },
    spec: {
      replicas: replicas,
      selector: { matchLabels: selector },
      template: {
        metadata: { labels: labels },
        spec: {
          containers: [{
            name: 'app',
            image: image,
            ports: [{ name: 'http', containerPort: port }],
            envFrom: [{ configMapRef: { name: configMapName } }],
            resources: { requests: { cpu: '10m', memory: '32Mi' } },
          }],
        },
      },
    },
  },

  service(name, labels, port):: {
    apiVersion: 'v1',
    kind: 'Service',
    metadata: { name: name, labels: labels },
    spec: {
      selector: { 'app.kubernetes.io/name': labels['app.kubernetes.io/name'] },
      ports: [{ name: 'http', port: port, targetPort: 'http' }],
    },
  },

  allowIngressPolicy(name, labels, port):: {
    apiVersion: 'networking.k8s.io/v1',
    kind: 'NetworkPolicy',
    metadata: { name: name + '-ingress', labels: labels },
    spec: {
      podSelector: { matchLabels: { 'app.kubernetes.io/name': labels['app.kubernetes.io/name'] } },
      policyTypes: ['Ingress'],
      ingress: [{ ports: [{ protocol: 'TCP', port: port }] }],
    },
  },
}
