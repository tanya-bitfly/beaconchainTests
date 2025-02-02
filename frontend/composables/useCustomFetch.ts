import type { NitroFetchOptions } from 'nitropack'
import type { RuntimeConfig } from 'nuxt/schema'
import {
  API_PATH, mapping, type PathValues,
} from '~/types/customFetch'

const APIcallTimeout = 30 * 1000 // 30 seconds

const pathNames = Object.values(API_PATH)
type PathName = (typeof pathNames)[number]

export function useCustomFetch() {
  const runtimeConfig = useRuntimeConfig()
  const getRuntimeConfigOnServerSide = (key: keyof RuntimeConfig['private']) => isServerSide
    ? runtimeConfig.private[key]
    : undefined
  const headers = useRequestHeaders([ 'cookie' ])
  const {
    setTokenCsrf,
    tokenCsrf,
  } = useCsrfStore()
  async function fetch<T>(
    pathName: PathName,
    // eslint-disable-next-line @typescript-eslint/ban-types
    options: NitroFetchOptions<{} & string> = {},
    pathValues?: PathValues,
    query?: PathValues,
  ): Promise<T> {
    const map = mapping[pathName]
    if (!map) {
      throw new Error(`path ${pathName} not found`)
    }

    if (options.signal === undefined) {
      options.signal = AbortSignal.timeout(APIcallTimeout)
    }

    if (map.mockFunction !== undefined && map.mock) {
      return map.mockFunction(options.body, pathValues, query) as T
    }

    const url = useRequestURL()
    const {
      public: {
        apiClient,
        apiKey,
        domain,
        legacyApiClient,
      },
    } = runtimeConfig
    const path = map.mock
      ? `${pathName}.json`
      : map.getPath?.(pathValues) || map.path
    let baseURL = map.mock
      ? '../mock'
      : map.legacy
        ? legacyApiClient
        : apiClient
    const legacyApiServer = getRuntimeConfigOnServerSide('legacyApiServer')
    const apiServer = getRuntimeConfigOnServerSide('apiServer')
    if (isServerSide) {
      baseURL = map.mock
        ? `${domain || url.origin.replace('http:', 'https:')}/mock`
        : map.legacy
          ? legacyApiServer ?? ''
          : apiServer ?? ''
    }

    options.headers = new Headers({
      ...options.headers,
      ...headers,
    })
    if (apiKey) {
      options.headers.append('Authorization', `Bearer ${apiKey}`)
    }

    const ssrSecret = getRuntimeConfigOnServerSide('ssrSecret')
    if (isServerSide && ssrSecret) {
      options.headers.append('x-ssr-secret', ssrSecret)
    }

    options.query = {
      ...options.query,
      ...query,
      is_mocked: runtimeConfig.public.isApiMocked ? true : undefined,
    }
    options.credentials = 'include'
    const method = options.method || map.method || 'GET'

    // For non GET method's we need to set the csrf header for security
    if (method !== 'GET') {
      if (tokenCsrf.value) {
        options.headers = new Headers({
          ...options.headers,
          'x-csrf-token': tokenCsrf.value,
        })
      }
    }

    const res = await $fetch.raw<T>(path, {
      baseURL,
      method,
      ...options,
    })
    if (method === 'GET') {
      // We get the csrf header from GET requests
      const tokenCsrf = res.headers.get('x-csrf-token')
      if (tokenCsrf) {
        setTokenCsrf(tokenCsrf)
      }
    }
    return res._data as T
  }

  return {
    fetch,
  }
}
