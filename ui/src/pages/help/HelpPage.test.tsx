import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Settings } from '../../api/types';
import { settingsFixture } from '../../mocks/fixtures';
import { helpSettings, helpVersion } from '../../mocks/fixtures/help';
import { problem } from '../../mocks/handlers';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';
import { chatLink, cliDownloads } from './help';

function withHelp(help: Settings['help'], version = helpVersion) {
  server.use(
    http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, help })),
    http.get('*/api/v1/version', () => HttpResponse.json(version)),
  );
}

describe('help page (API-109, API-111)', () => {
  it('lists the server binary and one download per configured platform, ignoring unknown keys', async () => {
    withHelp(helpSettings);
    renderApp('/help');
    expect(await screen.findByRole('heading', { level: 1, name: 'Docs & help' })).toBeInTheDocument();
    const list = await screen.findByRole('list', { name: 'CLI downloads' });
    await within(list).findByRole('link', { name: 'Linux (ARM64) from this server' });
    const links = within(list).getAllByRole('link');
    expect(links.map((l) => l.textContent?.replace(' (opens in a new tab)', ''))).toEqual([
      'Linux (ARM64) from this server',
      'macOS (ARM64)',
      'Windows (x86-64)',
      'Linux (x86-64)',
    ]);
    expect(links[0]).toHaveAttribute('href', 'download/patrol-linux');
    expect(links[0]).toHaveAttribute('download');
    expect(links[1]).toHaveAttribute('href', 'https://downloads.example.test/patrol-darwin-arm64');
    expect(links[1]).toHaveAttribute('rel', 'noopener noreferrer');
    expect(screen.queryByText(/plan9/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText('CLI sign-in command')).toHaveTextContent('patrol login patrol.example.test');
  });

  it('shows server and tool versions and copies them', async () => {
    const user = userEvent.setup();
    withHelp(helpSettings);
    renderApp('/help');
    const section = (await screen.findByRole('heading', { name: 'Version' })).closest('section');
    expect(section).not.toBeNull();
    const scoped = within(section!);
    expect(await scoped.findByText('v0.1.0+abc1234')).toBeInTheDocument();
    expect(scoped.getByText('linux/arm64')).toBeInTheDocument();
    expect(scoped.getByRole('heading', { name: 'Bundled tools' })).toBeInTheDocument();
    expect(scoped.getByText('v3.19.0')).toBeInTheDocument();
    expect(scoped.getByText('v5.8.1')).toBeInTheDocument();
    await user.click(scoped.getByRole('button', { name: 'Copy version information' }));
    expect(await navigator.clipboard.readText()).toContain('Helm: v3.19.0');
  });

  it('links to the configured chat with its text', async () => {
    withHelp(helpSettings);
    renderApp('/help');
    const link = await screen.findByRole('link', { name: /Ask the platform team/ });
    expect(link).toHaveAttribute('href', 'https://chat.example.test/patrol');
    expect(link).toHaveAttribute('target', '_blank');
  });

  it('disables the chat control for an invalid URL and explains why', async () => {
    withHelp({ chatUrl: 'javascript:alert(1)', chatText: '', binaryUrls: {} });
    renderApp('/help');
    const button = await screen.findByRole('button', { name: /Chat now!/ });
    expect(button).toHaveAttribute('aria-disabled', 'true');
    expect(button).toHaveTextContent('unavailable: invalid URL provided');
    expect(screen.queryByRole('link', { name: /Chat now!/ })).not.toBeInTheDocument();
  });

  it('omits the chat section when no chat URL is configured', async () => {
    withHelp({ chatUrl: '', chatText: '', binaryUrls: {} });
    renderApp('/help');
    await screen.findByRole('heading', { name: 'Version' });
    expect(screen.queryByRole('heading', { name: 'Get help' })).not.toBeInTheDocument();
  });

  it('keeps the page usable when version information fails to load', async () => {
    server.use(http.get('*/api/v1/version', () => problem(503, 'unavailable', 'patrol-server is restarting')));
    renderApp('/help');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Version information is unavailable');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'CLI downloads' })).toBeInTheDocument();
  });
});

describe('help helpers', () => {
  it('drops downloads with unsafe URLs', () => {
    const downloads = cliDownloads(
      { help: { binaryUrls: { 'linux-amd64': 'javascript:alert(1)', 'linux-arm64': 'https://dl.example.test/x' } } },
      { version: 'v0.0.0-test', platform: 'linux/amd64' },
    );
    expect(downloads.map((d) => d.id)).toEqual(['server', 'linux-arm64']);
    expect(downloads[0]?.label).toBe('Linux (x86-64) from this server');
  });

  it('parses title|url chat values and defaults the text', () => {
    expect(chatLink({ help: { chatUrl: 'Team chat|https://chat.example.test' } })).toEqual({
      text: 'Team chat',
      href: 'https://chat.example.test',
      raw: 'Team chat|https://chat.example.test',
    });
    expect(chatLink({ help: { chatUrl: 'https://chat.example.test' } })?.text).toBe('Chat now!');
    expect(chatLink({ help: { chatUrl: '' } })).toBeNull();
  });
});
