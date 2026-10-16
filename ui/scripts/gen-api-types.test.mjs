// @vitest-environment node
// Normalization of the published OpenAPI document for openapi-typescript (scripts/gen-api-types.mjs).
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { hoistedName, normalizeForTypes, publishedSpec, renderTypes } from './gen-api-types.mjs';

/** @type {Record<string, any>} */
const sample = {
  openapi: '3.1.0',
  info: { title: 't', version: '1' },
  paths: {},
  components: {
    schemas: {
      App: {
        type: 'object',
        properties: {
          spec: {
            type: 'object',
            properties: {
              source: { type: 'object', description: 'Source.', properties: { helm: { type: 'object', properties: { valuesObject: { type: 'object', 'x-kubernetes-preserve-unknown-fields': true } } } } },
              info: { type: 'array', items: { type: 'object', properties: { name: { type: 'string' } } } },
            },
          },
        },
      },
      SyncRequest: {
        type: 'object',
        properties: {
          source: { $ref: '#/components/schemas/App/properties/spec/properties/source' },
          helm: { $ref: '#/components/schemas/App/properties/spec/properties/source/properties/helm' },
          infos: { type: 'array', items: { $ref: '#/components/schemas/App/properties/spec/properties/info/items' } },
        },
      },
    },
  },
};

describe('gen-api-types', () => {
  it('names hoisted schemas after the pointer', () => {
    expect(hoistedName('App/properties/spec/properties/source')).toBe('AppSpecSource');
    expect(hoistedName('App/properties/spec/properties/info/items')).toBe('AppSpecInfoItem');
    expect(hoistedName('Project/properties/status/properties/jwtTokensByRole/additionalProperties')).toBe('ProjectStatusJwtTokensByRoleValue');
    expect(() => hoistedName('App/allOf/0')).toThrow(/allOf/);
  });

  it('hoists references into schemas (deepest first) and keeps the shapes', () => {
    const out = normalizeForTypes(sample);
    const schemas = out.components.schemas;
    expect(schemas.SyncRequest.properties.source).toEqual({ $ref: '#/components/schemas/AppSpecSource' });
    expect(schemas.SyncRequest.properties.helm).toEqual({ $ref: '#/components/schemas/AppSpecSourceHelm' });
    expect(schemas.SyncRequest.properties.infos.items).toEqual({ $ref: '#/components/schemas/AppSpecInfoItem' });
    // The original location now references the hoisted component (no duplicate inline copy).
    expect(schemas.App.properties.spec.properties.source).toEqual({ $ref: '#/components/schemas/AppSpecSource' });
    expect(schemas.AppSpecSource.properties.helm).toEqual({ $ref: '#/components/schemas/AppSpecSourceHelm' });
    expect(schemas.AppSpecSource.description).toBe('Source.');
    // Preserve-unknown objects become open maps instead of Record<string, never>.
    expect(schemas.AppSpecSourceHelm.properties.valuesObject.additionalProperties).toBe(true);
    // The input is not modified.
    expect(sample.components.schemas.SyncRequest.properties.source.$ref).toBe('#/components/schemas/App/properties/spec/properties/source');
  });

  it('rejects a hoisted name that collides with an existing schema', () => {
    const clash = structuredClone(sample);
    clash.components.schemas.AppSpecSource = { type: 'string' };
    expect(() => normalizeForTypes(clash)).toThrow(/collides/);
  });

  it('leaves no indexed-access types through properties in the generated file', async () => {
    const output = await renderTypes(JSON.parse(readFileSync(publishedSpec, 'utf8')), 'api/openapi/openapi.json');
    // components["schemas"]["X"]["spec"] would be any through optional properties and arrays.
    expect(output).not.toMatch(/components\["schemas"\]\["[A-Za-z_]+"\]\["/);
    expect(output).toContain('RepositoryInfo: {');
  });
});
