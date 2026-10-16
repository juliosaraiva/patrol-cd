#!/usr/bin/env node
// Generates ui/src/api/schema.d.ts with openapi-typescript from the OpenAPI document that
// patrol-server publishes (api/openapi/openapi.json, generated from the Go route table; see
// ADR-0005). The document is never edited here; it is normalized in memory for TypeScript only
// (normalizeForTypes below), so the generated types describe exactly the published contract.
//
// Flags: --spec <file|url>  another OpenAPI document (default ../api/openapi/openapi.json)
//        --out <file>       output file (default src/api/schema.d.ts)
//        --emit-spec <file> also write the normalized document that was fed to openapi-typescript
//        --check            exit 1 if the output file is stale instead of writing it

import { existsSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import openapiTS, { astToString } from 'openapi-typescript';
import { parse as parseYaml } from 'yaml';

const uiRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = path.resolve(uiRoot, '..');
export const publishedSpec = path.join(repoRoot, 'api', 'openapi', 'openapi.json');

/** @typedef {Record<string, any>} Json */

const SCHEMA_PREFIX = '#/components/schemas/';

/** @param {string} s */
const pascal = (s) => s.replace(/(^|[^A-Za-z0-9]+)([A-Za-z0-9])/g, (_m, _sep, c) => c.toUpperCase());

/**
 * Name of the component a JSON-pointer reference into a schema is hoisted to:
 * "Application/properties/spec/properties/source" -> "ApplicationSpecSource",
 * ".../items" -> "...Item", ".../additionalProperties" -> "...Value".
 * @param {string} pointer pointer below #/components/schemas/
 */
export function hoistedName(pointer) {
  const [root, ...rest] = pointer.split('/');
  let name = root ?? '';
  for (let i = 0; i < rest.length; i++) {
    const part = rest[i];
    if (part === 'properties') {
      name += pascal(rest[++i] ?? '');
    } else if (part === 'items') {
      name += 'Item';
    } else if (part === 'additionalProperties') {
      name += 'Value';
    } else {
      throw new Error(`cannot name a reference through "${part}" (${pointer})`);
    }
  }
  return name;
}

/** @param {unknown} node @param {(n: Json) => void} visit */
function walk(node, visit) {
  if (Array.isArray(node)) {
    for (const item of node) walk(item, visit);
  } else if (node !== null && typeof node === 'object') {
    visit(/** @type {Json} */ (node));
    for (const value of Object.values(node)) walk(value, visit);
  }
}

/** @param {string} part */
const unescapePointer = (part) => part.replace(/~1/g, '/').replace(/~0/g, '~');

/**
 * Prepares the published document for openapi-typescript. Two constructs of the server's
 * generator (CRD schemas embedded verbatim) do not survive the conversion otherwise:
 *
 * 1. References into a schema ("#/components/schemas/Application/properties/spec/properties/source")
 *    become indexed-access types (`components["schemas"]["Application"]["spec"]["source"]`) that
 *    are invalid through optional properties and arrays, so they silently resolve to `any`. Each
 *    such target is hoisted into its own component (named by hoistedName) and replaced in place
 *    by a reference to it; the shape is unchanged.
 * 2. `x-kubernetes-preserve-unknown-fields` objects without properties would become
 *    `Record<string, never>`; they get `additionalProperties: true` (an open map).
 *
 * @param {Json} input the published document (not modified)
 * @returns {Json} a normalized copy
 */
export function normalizeForTypes(input) {
  const spec = structuredClone(input);
  const schemas = /** @type {Json} */ (spec.components?.schemas ?? {});

  /** @type {Set<string>} */
  const pointers = new Set();
  walk(spec, (node) => {
    const ref = node.$ref;
    if (typeof ref === 'string' && ref.startsWith(SCHEMA_PREFIX) && ref.slice(SCHEMA_PREFIX.length).includes('/')) {
      pointers.add(ref.slice(SCHEMA_PREFIX.length));
    }
  });

  // Deepest first: replacing a deeper node never invalidates the path to a shallower one.
  const ordered = [...pointers].sort((a, b) => b.split('/').length - a.split('/').length || a.localeCompare(b));
  /** @type {Map<string, string>} */
  const names = new Map();
  for (const pointer of ordered) {
    const name = hoistedName(pointer);
    if (name in schemas) throw new Error(`hoisted schema name ${name} (${pointer}) collides with an existing schema`);
    const parts = pointer.split('/').map(unescapePointer);
    /** @type {any} */
    let parent = schemas;
    for (const part of parts.slice(0, -1)) {
      parent = parent?.[part];
      if (parent === undefined) throw new Error(`reference target ${pointer} not found`);
    }
    const last = /** @type {string} */ (parts.at(-1));
    const target = parent?.[last];
    if (target === undefined || target === null || typeof target !== 'object') {
      throw new Error(`reference target ${pointer} not found`);
    }
    schemas[name] = target;
    parent[last] = { $ref: `${SCHEMA_PREFIX}${name}` };
    names.set(pointer, name);
  }
  walk(spec, (node) => {
    const ref = node.$ref;
    if (typeof ref !== 'string' || !ref.startsWith(SCHEMA_PREFIX)) return;
    const name = names.get(ref.slice(SCHEMA_PREFIX.length));
    if (name) node.$ref = `${SCHEMA_PREFIX}${name}`;
  });

  walk(schemas, (node) => {
    if (
      node['x-kubernetes-preserve-unknown-fields'] === true &&
      node.type === 'object' &&
      !node.properties &&
      node.additionalProperties === undefined
    ) {
      node.additionalProperties = true;
    }
  });
  return spec;
}

const HEADER = `/**
 * GENERATED FILE - DO NOT EDIT. Run \`pnpm -C ui gen:api\`.
 * Source: %SOURCE% (normalized by ui/scripts/gen-api-types.mjs)
 */

`;

/**
 * @param {Json} spec the published document
 * @param {string} source description for the header
 */
export async function renderTypes(spec, source) {
  const ast = await openapiTS(/** @type {import('openapi-typescript').OpenAPI3} */ (normalizeForTypes(spec)), {
    defaultNonNullable: false,
    alphabetize: false,
    silent: true,
  });
  return HEADER.replace('%SOURCE%', source) + astToString(ast);
}

async function main() {
  const { values } = parseArgs({
    options: {
      spec: { type: 'string' },
      out: { type: 'string', default: path.join(uiRoot, 'src', 'api', 'schema.d.ts') },
      'emit-spec': { type: 'string' },
      check: { type: 'boolean', default: false },
    },
  });

  const specArg = values.spec ?? publishedSpec;
  const isUrl = /^https?:\/\//.test(specArg);
  if (!isUrl && !existsSync(path.resolve(specArg))) {
    throw new Error(`OpenAPI document ${specArg} not found; run \`make generate\` (or pass --spec)`);
  }
  const raw = isUrl ? await (await fetch(specArg)).text() : await readFile(path.resolve(specArg), 'utf8');
  /** @type {Json} */
  const spec = specArg.endsWith('.json') || raw.trimStart().startsWith('{') ? JSON.parse(raw) : parseYaml(raw);
  const source = isUrl ? specArg : path.relative(repoRoot, path.resolve(specArg));

  const output = await renderTypes(spec, source);
  const outPath = path.resolve(values.out);

  if (values['emit-spec']) {
    await writeFile(path.resolve(values['emit-spec']), `${JSON.stringify(normalizeForTypes(spec), null, 2)}\n`);
  }
  if (values.check) {
    const current = existsSync(outPath) ? await readFile(outPath, 'utf8') : '';
    if (current !== output) {
      console.error(`${path.relative(uiRoot, outPath)} is stale relative to ${source}; run pnpm -C ui gen:api`);
      process.exit(1);
    }
    console.log(`${path.relative(uiRoot, outPath)} is up to date (source: ${source})`);
    return;
  }
  await mkdir(path.dirname(outPath), { recursive: true });
  await writeFile(outPath, output);
  const pathCount = Object.keys(spec.paths ?? {}).length;
  const schemaCount = Object.keys(spec.components?.schemas ?? {}).length;
  console.log(`wrote ${path.relative(uiRoot, outPath)}: ${pathCount} paths, ${schemaCount} schemas (source: ${source})`);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((err) => {
    console.error(err);
    process.exit(1);
  });
}
