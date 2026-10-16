/**
 * Test helpers over the published OpenAPI document (api/openapi/openapi.json): operation lookup,
 * request matching against path templates, and a small JSON Schema validator for the keywords
 * the document uses (type, properties, required, additionalProperties, items, enum, anyOf, allOf,
 * $ref, format date-time, pattern, min/max length/items, minimum). Tests only; never bundled.
 *
 * `strict` validation also rejects properties a schema does not declare (unless the schema
 * allows additional properties or preserves unknown fields), which catches fixtures and request
 * bodies that use stale field names even though the document leaves objects open.
 */
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
export type Schema = Record<string, unknown>;

export interface ParameterObject {
  name: string;
  in: 'path' | 'query' | 'header' | 'cookie';
  required?: boolean;
  schema?: Schema;
  'x-patrol-multi-segment'?: boolean;
}

export interface MediaObject {
  schema?: Schema;
}

export interface ResponseObject {
  $ref?: string;
  description?: string;
  content?: Record<string, MediaObject>;
}

export interface OperationObject {
  operationId: string;
  parameters?: ParameterObject[];
  requestBody?: { content?: Record<string, MediaObject> };
  responses: Record<string, ResponseObject>;
  'x-patrol-stream'?: boolean;
}

export interface OpenAPIDocument {
  paths: Record<string, Record<string, OperationObject>>;
  components: { schemas: Record<string, Schema>; responses?: Record<string, ResponseObject> };
}

export const HTTP_METHODS = ['get', 'put', 'post', 'patch', 'delete'] as const;
export type HttpMethod = (typeof HTTP_METHODS)[number];

export interface SpecOperation {
  operationId: string;
  method: Uppercase<HttpMethod>;
  path: string;
  operation: OperationObject;
}

let cached: OpenAPIDocument | undefined;

/** The published document, read from the repository (not the generated types). */
export function loadSpec(): OpenAPIDocument {
  const file = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../../api/openapi/openapi.json');
  cached ??= JSON.parse(readFileSync(file, 'utf8')) as OpenAPIDocument;
  return cached;
}

export function specOperations(spec: OpenAPIDocument = loadSpec()): SpecOperation[] {
  const out: SpecOperation[] = [];
  for (const [path, item] of Object.entries(spec.paths)) {
    for (const method of HTTP_METHODS) {
      const operation = item[method];
      if (operation) out.push({ operationId: operation.operationId, method: method.toUpperCase() as Uppercase<HttpMethod>, path, operation });
    }
  }
  return out;
}

export function findOperation(operationId: string, spec: OpenAPIDocument = loadSpec()): SpecOperation {
  const found = specOperations(spec).find((o) => o.operationId === operationId);
  if (!found) throw new Error(`operationId ${operationId} is not in the published document`);
  return found;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Matches a request path (percent-encoded, relative to the root path) against an operation's
 * path template. Parameters marked x-patrol-multi-segment may span "/"; the others are one
 * non-empty segment. Returns the decoded parameters, or null.
 */
export function matchPath(op: SpecOperation, pathname: string): Record<string, string> | null {
  const multi = new Set((op.operation.parameters ?? []).filter((p) => p.in === 'path' && p['x-patrol-multi-segment']).map((p) => p.name));
  const names: string[] = [];
  const source = op.path
    .split(/(\{[^}]+\})/)
    .map((part) => {
      const m = /^\{([^}]+)\}$/.exec(part);
      if (!m?.[1]) return escapeRegExp(part);
      names.push(m[1]);
      return multi.has(m[1]) ? '(.+)' : '([^/]+)';
    })
    .join('');
  const match = new RegExp(`^${source}$`).exec(pathname);
  if (!match) return null;
  return Object.fromEntries(names.map((n, i) => [n, decodeURIComponent(match[i + 1] ?? '')]));
}

/** Operation (of all in the document) whose method and path template match a request path. */
export function operationFor(method: string, pathname: string, spec: OpenAPIDocument = loadSpec()): SpecOperation | undefined {
  const candidates = specOperations(spec).filter((o) => o.method === method.toUpperCase() && matchPath(o, pathname));
  // Prefer literal segments over parameters (e.g. /applicationsets/generate over /applicationsets/{name}).
  return candidates.sort((a, b) => b.path.replace(/\{[^}]+\}/g, '').length - a.path.replace(/\{[^}]+\}/g, '').length)[0];
}

export function queryParameterNames(op: SpecOperation): Set<string> {
  return new Set((op.operation.parameters ?? []).filter((p) => p.in === 'query').map((p) => p.name));
}

function resolve(spec: OpenAPIDocument, ref: string): unknown {
  if (!ref.startsWith('#/')) throw new Error(`external reference ${ref}`);
  let node: unknown = spec;
  for (const raw of ref.slice(2).split('/')) {
    const part = raw.replace(/~1/g, '/').replace(/~0/g, '~');
    node = node !== null && typeof node === 'object' ? (node as Record<string, unknown>)[part] : undefined;
    if (node === undefined) throw new Error(`unresolvable reference ${ref}`);
  }
  return node;
}

function responseObject(spec: OpenAPIDocument, op: SpecOperation, status: string): ResponseObject | undefined {
  const r = op.operation.responses[status];
  if (!r) return undefined;
  return r.$ref ? (resolve(spec, r.$ref) as ResponseObject) : r;
}

/** Schema of a response body for a status and media type ("default" for problem documents). */
export function responseSchema(op: SpecOperation, status: number, mediaType: string, spec: OpenAPIDocument = loadSpec()): Schema | undefined {
  const r = responseObject(spec, op, String(status)) ?? (status >= 400 ? responseObject(spec, op, 'default') : undefined);
  return r?.content?.[mediaType]?.schema;
}

export function declaredResponseStatuses(op: SpecOperation): string[] {
  return Object.keys(op.operation.responses);
}

export function requestSchema(op: SpecOperation, mediaType: string): Schema | undefined {
  return op.operation.requestBody?.content?.[mediaType]?.schema;
}

export interface ValidateOptions {
  /** Reject properties a closed object schema does not declare. */
  strict?: boolean;
}

function typeOf(value: unknown): string {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'array';
  if (typeof value === 'number') return Number.isInteger(value) ? 'integer' : 'number';
  return typeof value;
}

function typeMatches(expected: string, value: unknown): boolean {
  const actual = typeOf(value);
  return actual === expected || (expected === 'number' && actual === 'integer');
}

/** Validates `value` against `schema`; returns human-readable errors ("" path is the root). */
export function validateSchema(schema: Schema, value: unknown, options: ValidateOptions = {}, spec: OpenAPIDocument = loadSpec()): string[] {
  const errors: string[] = [];
  const walk = (s: Schema, v: unknown, at: string) => {
    if (typeof s.$ref === 'string') {
      walk(resolve(spec, s.$ref) as Schema, v, at);
      return;
    }
    if (Array.isArray(s.anyOf)) {
      const branches = s.anyOf as Schema[];
      if (!branches.some((b) => validateSchema(b, v, options, spec).length === 0)) errors.push(`${at || '/'}: matches no anyOf branch`);
      return;
    }
    // Every allOf branch applies, then the schema's own keywords (e.g. Stored<Kind>: the CRD
    // schema plus `required: [metadata]`).
    if (Array.isArray(s.allOf)) for (const b of s.allOf as Schema[]) walk(b, v, at);
    const types = s.type === undefined ? [] : Array.isArray(s.type) ? (s.type as string[]) : [s.type as string];
    if (types.length > 0 && !types.some((t) => typeMatches(t, v))) {
      errors.push(`${at || '/'}: expected ${types.join('|')}, got ${typeOf(v)}`);
      return;
    }
    if (Array.isArray(s.enum) && !(s.enum as unknown[]).includes(v)) {
      errors.push(`${at || '/'}: ${JSON.stringify(v)} is not one of ${JSON.stringify(s.enum)}`);
    }
    if (typeof v === 'string') {
      if (typeof s.minLength === 'number' && v.length < s.minLength) errors.push(`${at}: shorter than ${s.minLength}`);
      if (typeof s.maxLength === 'number' && v.length > s.maxLength) errors.push(`${at}: longer than ${s.maxLength}`);
      if (typeof s.pattern === 'string' && !new RegExp(s.pattern).test(v)) errors.push(`${at}: does not match ${s.pattern}`);
      if (s.format === 'date-time' && Number.isNaN(Date.parse(v))) errors.push(`${at}: not a date-time`);
    }
    if (typeof v === 'number' && typeof s.minimum === 'number' && v < s.minimum) errors.push(`${at}: below ${s.minimum}`);
    if (Array.isArray(v)) {
      if (typeof s.minItems === 'number' && v.length < s.minItems) errors.push(`${at}: fewer than ${s.minItems} items`);
      if (typeof s.maxItems === 'number' && v.length > s.maxItems) errors.push(`${at}: more than ${s.maxItems} items`);
      if (s.items && typeof s.items === 'object') v.forEach((item, i) => walk(s.items as Schema, item, `${at}/${i}`));
    }
    if (typeOf(v) === 'object') {
      const obj = v as Record<string, unknown>;
      const props = (s.properties ?? undefined) as Record<string, Schema> | undefined;
      for (const key of (s.required as string[] | undefined) ?? []) {
        if (!(key in obj)) errors.push(`${at || '/'}: missing required property "${key}"`);
      }
      for (const [key, child] of Object.entries(obj)) {
        const declared = props?.[key];
        if (declared) {
          walk(declared, child, `${at}/${key}`);
        } else if (s.additionalProperties && typeof s.additionalProperties === 'object') {
          walk(s.additionalProperties as Schema, child, `${at}/${key}`);
        } else if (s.additionalProperties === false) {
          errors.push(`${at}/${key}: property not allowed`);
        } else if (options.strict && props && s.additionalProperties !== true && s['x-kubernetes-preserve-unknown-fields'] !== true) {
          errors.push(`${at}/${key}: property not declared by the schema`);
        }
      }
    }
  };
  walk(schema, value, '');
  return errors;
}

/** Splits a text/event-stream body into events (name and data). */
export function parseEventStream(text: string): { event: string; data: string; id: string | undefined }[] {
  const events: { event: string; data: string; id: string | undefined }[] = [];
  for (const block of text.split(/\r?\n\r?\n/)) {
    const data: string[] = [];
    let event = 'message';
    let id: string | undefined;
    for (const line of block.split(/\r?\n/)) {
      if (line.startsWith(':') || line === '') continue;
      const colon = line.indexOf(':');
      const field = colon === -1 ? line : line.slice(0, colon);
      const value = colon === -1 ? '' : line.slice(colon + 1).replace(/^ /, '');
      if (field === 'data') data.push(value);
      else if (field === 'event') event = value;
      else if (field === 'id') id = value;
    }
    if (data.length > 0) events.push({ event, data: data.join('\n'), id });
  }
  return events;
}
