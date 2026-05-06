import { readFileSync, existsSync } from 'fs';
import { homedir } from 'os';
import { join, resolve } from 'path';

export interface SentryConfig {
  url: string;
  token: string;
  org: string;
}

export interface Config {
  sentry?: SentryConfig;
}

interface ConfigFile {
  sentry?: { url?: string; token?: string; org?: string };
}

function readJsonFile(filePath: string): ConfigFile | null {
  try {
    if (!existsSync(filePath)) return null;
    return JSON.parse(readFileSync(filePath, 'utf-8')) as ConfigFile;
  } catch {
    return null;
  }
}

function getConfigPath(): string | null {
  const configArgIndex = process.argv.indexOf('--config');
  if (configArgIndex !== -1 && process.argv[configArgIndex + 1]) {
    return resolve(process.argv[configArgIndex + 1]);
  }
  if (process.env.SENTRY_MCP_CONFIG) {
    return resolve(process.env.SENTRY_MCP_CONFIG);
  }
  const homeConfig = join(homedir(), '.sentry-mcp.json');
  if (existsSync(homeConfig)) return homeConfig;
  const cwdConfig = join(process.cwd(), '.sentry-mcp.json');
  if (existsSync(cwdConfig)) return cwdConfig;
  return null;
}

export function loadConfig(): Config {
  const configPath = getConfigPath();
  const file = configPath ? readJsonFile(configPath) : null;

  const url = file?.sentry?.url ?? process.env.SENTRY_URL ?? '';
  const token = file?.sentry?.token ?? process.env.SENTRY_AUTH_TOKEN ?? '';
  const org = file?.sentry?.org ?? process.env.SENTRY_ORG_SLUG ?? '';
  const config: Config = {};

  if (url && token && org) {
    try {
      new URL(url);
    } catch {
      console.error(`[sentry-mcp] Invalid SENTRY_URL: ${url}`);
      return config;
    }
    config.sentry = { url: url.replace(/\/$/, ''), token, org };
  } else if (url || token || org) {
    const missing: string[] = [];
    if (!url) missing.push('sentry.url (or SENTRY_URL)');
    if (!token) missing.push('sentry.token (or SENTRY_AUTH_TOKEN)');
    if (!org) missing.push('sentry.org (or SENTRY_ORG_SLUG)');
    console.error(`[sentry-mcp] Sentry disabled: missing ${missing.join(', ')}`);
  }

  return config;
}
