{
  name: 'guestbook',
  image: 'ghcr.io/stefanprodan/podinfo',
  port: 9898,
  colors: {
    dev: '#34577c',
    staging: '#7c5a34',
    prod: '#1f4e3d',
  },
  defaults: {
    networkPolicy: false,
  },
}
