#!/usr/bin/env node
// Generates ui/src/api/schema.d.ts with openapi-typescript.
//
// Source selection (first match wins):
//   1. --spec <file|url>                 an explicit OpenAPI document
//   2. ../api/openapi/openapi.json       the document patrol-server publishes (ADR-0005), once
//                                        it exists; pass --from-crds to ignore it
//   3. a temporary document built from deploy/crds/*.yaml (CRD openAPIV3Schema for every
//      Patrol kind) plus the hand-declared schemas and routes in scripts/api-schemas.mjs and
//      scripts/api-routes.mjs (mirrors docs/architecture/api-contract.md)
//
// Flags: --out <file> (default src/api/schema.d.ts), --crds <dir> (default ../deploy/crds),
//        --emit-spec <file> (also write the OpenAPI document that was used),
//        --check (exit 1 if the output file is stale instead of writing it).

import { existsSync } from 'node:fs';
import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import openapiTS, { astToString } from 'openapi-typescript';
import { parse as parseYaml, parseAllDocuments } from 'yaml';
import { routes } from './api-routes.mjs';
import { handSchemas, listSchemas, objectMetaRef } from './api-schemas.mjs';

const uiRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = path.resolve(uiRoot, '..');
const publishedSpec = path.join(repoRoot, 'api', 'openapi', 'openapi.json');

/** Kinds exported as components.schemas, in CRD order. */
export const CRD_KINDS = [
  'Application',
  'Operation',
  'AppProject',
  'ApplicationSet',
  'Repository',
  'RepositoryCredential',
  'Cluster',
  'SCMProvider',
];

/** @typedef {Record<string, any>} Json */

/**
 * Strips Kubernetes vendor extensions that openapi-typescript does not understand and maps
 * the ones with typing consequences onto plain OpenAPI.
 * @param {unknown} node
 * @returns {any}
 */
export function convertCrdSchema(node) {
  if (Array.isArray(node)) return node.map(convertCrdSchema);
  if (node === null || typeof node !== 'object') return node;
  const src = /** @type {Json} */ (node);
  /** @type {Json} */
  const out = {};
  for (const [key, value] of Object.entries(src)) {
    if (key.startsWith('x-kubernetes-')) continue;
    if (key === 'properties' || key === 'patternProperties' || key === 'definitions') {
      out[key] = Object.fromEntries(
        Object.entries(/** @type {Json} */ (value)).map(([k, v]) => [k, convertCrdSchema(v)]),
      );
      continue;
    }
    out[key] = convertCrdSchema(value);
  }
  if (src['x-kubernetes-int-or-string'] === true) {
    delete out.type;
    delete out.anyOf;
    out.anyOf = [{ type: 'integer' }, { type: 'string' }];
  }
  if (src['x-kubernetes-preserve-unknown-fields'] === true && !src.properties && !src.items) {
    if (src.type === 'object' && out.additionalProperties === undefined) out.additionalProperties = true;
  }
  return out;
}

/**
 * @param {string} crdDir
 * @returns {Promise<Map<string, Json>>} kind -> converted openAPIV3Schema (storage version)
 */
export async function loadCrdSchemas(crdDir) {
  const files = (await readdir(crdDir)).filter((f) => f.endsWith('.yaml') || f.endsWith('.yml')).sort();
  /** @type {Map<string, Json>} */
  const byKind = new Map();
  const docs = [];
  for (const file of files) {
    for (const parsed of parseAllDocuments(await readFile(path.join(crdDir, file), 'utf8'))) {
      if ('errors' in parsed && parsed.errors.length > 0) throw new Error(`${file}: ${parsed.errors[0]?.message}`);
      docs.push(parsed.toJS());
    }
  }
  for (const doc of docs) {
    if (doc?.kind !== 'CustomResourceDefinition') continue;
    const kind = doc.spec?.names?.kind;
    /** @type {Json[]} */
    const versions = doc.spec?.versions ?? [];
    const version = versions.find((v) => v.storage) ?? versions[0];
    const schema = version?.schema?.openAPIV3Schema;
    if (typeof kind !== 'string' || !schema) continue;
    const converted = convertCrdSchema(schema);
    converted.properties = { ...converted.properties, metadata: objectMetaRef };
    converted.required = [...new Set([...(converted.required ?? []), 'metadata'])];
    converted['x-patrol-group-version'] = `${doc.spec.group}/${version.name}`;
    byKind.set(kind, converted);
  }
  return byKind;
}

/**
 * @param {Json} root
 * @param {string} pointer JSON pointer such as /properties/spec
 * @returns {Json}
 */
function resolvePointer(root, pointer) {
  /** @type {any} */
  let node = root;
  for (const part of pointer.split('/').slice(1)) {
    node = node?.[part.replace(/~1/g, '/').replace(/~0/g, '~')];
    if (node === undefined) throw new Error(`pointer ${pointer} not found`);
  }
  return structuredClone(node);
}

/** @param {string} kind */
function responseFor(kind) {
  if (kind === 'empty') return { 200: { description: 'OK', content: { 'application/json': { schema: { $ref: '#/components/schemas/Empty' } } } } };
  if (kind === 'redirect') return { 302: { description: 'Redirect' } };
  if (kind === 'websocket') return { 101: { description: 'Switching protocols (WebSocket); frames are TerminalMessage JSON' } };
  if (kind === 'svg') return { 200: { description: 'SVG badge', content: { 'image/svg+xml': { schema: { type: 'string' } } } } };
  if (kind.startsWith('sse:')) {
    const name = kind.slice(4);
    return { 200: { description: `text/event-stream; each data line is a ${name} JSON document`, content: { 'text/event-stream': { schema: { $ref: `#/components/schemas/${name}` } } } } };
  }
  return { 200: { description: 'OK', content: { 'application/json': { schema: { $ref: `#/components/schemas/${kind}` } } } } };
}

/** @param {string} body */
function requestBodyFor(body) {
  if (body === 'gzip') {
    return { required: true, content: { 'application/gzip': { schema: { type: 'string', format: 'binary' } } } };
  }
  return { required: true, content: { 'application/json': { schema: { $ref: `#/components/schemas/${body}` } } } };
}

/** @param {Map<string, Json>} crds */
export function buildSpec(crds) {
  for (const kind of CRD_KINDS) {
    if (!crds.has(kind)) throw new Error(`CRD for kind ${kind} not found`);
  }
  /** @param {string} kind @param {string} pointer */
  const crd = (kind, pointer) => {
    const schema = crds.get(kind);
    if (!schema) throw new Error(`unknown CRD kind ${kind}`);
    return resolvePointer(schema, pointer);
  };

  /** @type {Json} */
  const schemas = {};
  for (const kind of CRD_KINDS) schemas[kind] = crds.get(kind);
  Object.assign(schemas, listSchemas(CRD_KINDS), handSchemas(crd));

  /** @type {Json} */
  const paths = {};
  for (const route of routes) {
    const params = [...route.path.matchAll(/\{([^}]+)\}/g)].map((m) => ({
      name: m[1],
      in: 'path',
      required: true,
      schema: { type: 'string' },
    }));
    for (const qp of route.query ?? []) {
      params.push({
        name: qp.name,
        in: 'query',
        required: false,
        schema: qp.schema,
        ...(qp.description ? { description: qp.description } : {}),
        ...(qp.schema.type === 'array' ? { style: 'form', explode: true } : {}),
      });
    }
    /** @type {Json} */
    const operation = {
      operationId: route.operationId,
      summary: route.summary,
      tags: [route.tag],
      ...(params.length > 0 ? { parameters: params } : {}),
      ...(route.body ? { requestBody: requestBodyFor(route.body) } : {}),
      responses: {
        ...responseFor(route.response),
        default: {
          description: 'Error',
          content: { 'application/problem+json': { schema: { $ref: '#/components/schemas/Problem' } } },
        },
      },
    };
    paths[route.path] ??= {};
    const method = route.method.toLowerCase();
    if (paths[route.path][method]) throw new Error(`duplicate route ${route.method} ${route.path}`);
    paths[route.path][method] = operation;
  }

  return {
    openapi: '3.0.3',
    info: {
      title: 'Patrol CD API (provisional, generated by ui/scripts/gen-api-types.mjs)',
      version: '0.0.0',
      description:
        'Temporary document built from deploy/crds and docs/architecture/api-contract.md until patrol-server publishes api/openapi/openapi.json.',
    },
    paths,
    components: { schemas },
  };
}

const HEADER = `/**
 * GENERATED FILE - DO NOT EDIT. Run \`pnpm -C ui gen:api\`.
 * Source: %SOURCE%
 */

`;

async function main() {
  const { values } = parseArgs({
    options: {
      spec: { type: 'string' },
      'from-crds': { type: 'boolean', default: false },
      crds: { type: 'string', default: path.join(repoRoot, 'deploy', 'crds') },
      out: { type: 'string', default: path.join(uiRoot, 'src', 'api', 'schema.d.ts') },
      'emit-spec': { type: 'string' },
      check: { type: 'boolean', default: false },
    },
  });

  /** @type {Json} */
  let spec;
  let source;
  if (values.spec) {
    const isUrl = /^https?:\/\//.test(values.spec);
    const raw = isUrl
      ? await (await fetch(values.spec)).text()
      : await readFile(path.resolve(values.spec), 'utf8');
    spec = values.spec.endsWith('.json') || raw.trimStart().startsWith('{') ? JSON.parse(raw) : parseYaml(raw);
    source = isUrl ? values.spec : path.relative(repoRoot, path.resolve(values.spec));
  } else if (!values['from-crds'] && existsSync(publishedSpec)) {
    spec = JSON.parse(await readFile(publishedSpec, 'utf8'));
    source = path.relative(repoRoot, publishedSpec);
  } else {
    spec = buildSpec(await loadCrdSchemas(path.resolve(values.crds)));
    source = `${path.relative(repoRoot, path.resolve(values.crds))}/*.yaml + ui/scripts/api-schemas.mjs + ui/scripts/api-routes.mjs`;
  }

  const ast = await openapiTS(spec, {
    defaultNonNullable: false,
    alphabetize: false,
    silent: true,
  });
  const output = HEADER.replace('%SOURCE%', source) + astToString(ast);
  const outPath = path.resolve(values.out);

  if (values['emit-spec']) {
    await writeFile(path.resolve(values['emit-spec']), `${JSON.stringify(spec, null, 2)}\n`);
  }
  if (values.check) {
    const current = existsSync(outPath) ? await readFile(outPath, 'utf8') : '';
    if (current !== output) {
      console.error(`${path.relative(uiRoot, outPath)} is stale; run pnpm -C ui gen:api`);
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
