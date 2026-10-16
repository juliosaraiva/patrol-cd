import type { Settings, Version } from '../../api/types';

/** help.* settings with every supported CLI download plus one unsupported key. */
export const helpSettings: Settings['help'] = {
  chatUrl: 'https://chat.example.test/patrol',
  chatText: 'Ask the platform team',
  binaryUrls: {
    'darwin-arm64': 'https://downloads.example.test/patrol-darwin-arm64',
    'linux-amd64': 'https://downloads.example.test/patrol-linux-amd64',
    'windows-amd64': 'https://downloads.example.test/patrol-windows-amd64.exe',
    'plan9-mips': 'https://downloads.example.test/patrol-plan9',
  },
};

export const helpVersion: Version = {
  version: 'v0.1.0+abc1234',
  buildDate: '2026-10-14T00:00:00Z',
  gitCommit: 'abc1234def5678abc1234def5678abc1234def56',
  gitTreeState: 'clean',
  goVersion: 'go1.25.3',
  compiler: 'gc',
  platform: 'linux/arm64',
  kustomizeVersion: 'v5.8.1',
  helmVersion: 'v3.19.0',
  kubectlVersion: 'v0.34.1',
  jsonnetVersion: 'v0.21.0',
};
