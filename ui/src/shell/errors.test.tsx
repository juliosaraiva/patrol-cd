import { render, screen } from '@testing-library/react';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import { describeError, RouteError } from './errors';

function Boom({ error }: { error: unknown }): never {
  throw error;
}

function renderRouteError(error: unknown) {
  const router = createMemoryRouter([{ path: '/', element: <Boom error={error} />, errorElement: <RouteError /> }]);
  render(<RouterProvider router={router} />);
}

describe('describeError', () => {
  it.each([
    [new ApiError({ status: 401 }), 'Your session has ended', 'login'],
    [new ApiError({ status: 403, detail: 'denied' }), 'You do not have access', 'navigate'],
    [new ApiError({ status: 404, detail: 'application "x" not found' }), 'Not found', 'navigate'],
    [new ApiError({ status: 0 }), 'Patrol server unavailable', 'retry'],
    [new ApiError({ status: 409, detail: 'changed' }), 'The object changed', 'retry'],
    [new ApiError({ status: 500, detail: 'boom' }), 'Server error', 'retry'],
    // Server problem titles are reason phrases; the code names the situation.
    [new ApiError({ status: 400, code: 'invalid_argument', title: 'Bad Request', detail: 'spec is invalid' }), 'The request was rejected', 'navigate'],
    [new ApiError({ status: 409, code: 'already_exists', title: 'Conflict', detail: 'exists' }), 'It already exists', 'navigate'],
    [new ApiError({ status: 412, code: 'failed_precondition', title: 'Precondition Failed', detail: 'in use' }), 'Not possible in the current state', 'navigate'],
    [new Error('render bug'), 'Something went wrong', 'contact'],
  ])('%s → %s', (error, title, recovery) => {
    expect(describeError(error)).toMatchObject({ title, recovery });
  });
});

describe('RouteError', () => {
  it('offers sign-in for expired sessions', async () => {
    renderRouteError(new ApiError({ status: 401 }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Your session has ended' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Sign in again' }).getAttribute('href')).toMatch(/^\/login\?return_url=/);
  });

  it('shows technical details and a reload action for unexpected errors', async () => {
    renderRouteError(new Error('render bug'));
    expect(await screen.findByRole('heading', { level: 1, name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload page' })).toBeInTheDocument();
    expect(screen.getByText('Technical details')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to applications' })).toBeInTheDocument();
  });
});
