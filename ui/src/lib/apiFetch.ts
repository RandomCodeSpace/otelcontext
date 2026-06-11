// The one fetch wrapper for every REST call in the UI. TanStack Query hands
// each queryFn an AbortSignal; passing it through here is what cancels
// in-flight requests on unmount/refetch (the audit's leak finding).
//
// Error normalization contract:
//   - non-2xx            → ApiError { status: <http status> }
//   - network failure    → ApiError { status: 0 }
//   - unparseable body   → ApiError { status: <http status> }
//   - AbortError         → re-thrown untouched so Query can ignore it

export class ApiError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export interface ApiFetchOptions {
  signal?: AbortSignal
  headers?: Record<string, string>
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError'
}

/** Fetch + parse JSON, returning the Response for header access (X-Cache). */
export async function apiFetchWithResponse<T>(
  path: string,
  options: ApiFetchOptions = {},
): Promise<{ data: T; response: Response }> {
  let response: Response
  try {
    response = await fetch(path, {
      signal: options.signal,
      headers: options.headers,
    })
  } catch (err: unknown) {
    if (isAbort(err)) throw err
    const message = err instanceof Error ? err.message : 'network error'
    throw new ApiError(message, 0)
  }
  if (!response.ok) {
    throw new ApiError(`HTTP ${response.status} for ${path}`, response.status)
  }
  let data: T
  try {
    data = (await response.json()) as T
  } catch (err: unknown) {
    if (isAbort(err)) throw err
    throw new ApiError(`invalid JSON from ${path}`, response.status)
  }
  return { data, response }
}

/** Fetch + parse JSON. The default entry point for queryFns. */
export async function apiFetch<T>(
  path: string,
  options: ApiFetchOptions = {},
): Promise<T> {
  const { data } = await apiFetchWithResponse<T>(path, options)
  return data
}
