export const CAPABILITY_LABELS: Record<string, string> = { login: 'Login', apiKey: 'API key' };

export const accountPath = (name: string) => `/settings/accounts/${encodeURIComponent(name)}`;
