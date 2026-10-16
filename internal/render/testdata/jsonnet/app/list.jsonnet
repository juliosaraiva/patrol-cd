local k = import 'k.libsonnet';
{ apiVersion: 'v1', kind: 'List', items: [k.cm('l1'), k.cm('l2')] }
