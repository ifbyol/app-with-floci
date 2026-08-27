export interface Movie {
  id: string;
  title: string;
  year: number;
  genre: string;
  director: string;
  synopsis: string;
  rating: number;
  createdAt: string;
}

export interface Hit extends Movie {
  score: number;
}

export interface Facet {
  key: string;
  count: number;
}

export interface SearchResults {
  total: number;
  tookMillis: number;
  hits: Hit[];
  genres: Facet[];
}

export interface CacheInfo {
  hit: boolean;
  source: string;
  lookupMicros: number;
  ttlSeconds: number;
}

export interface MovieDetail {
  movie: Movie;
  cache: CacheInfo;
}

export interface ServiceState {
  service: string;
  api: string;
  resource: string;
  status: string;
  ready: boolean;
  advertised: string;
  effective: string;
  note?: string;
}

export interface AwsStatus {
  flociEndpoint: string;
  appReady: boolean;
  ready: boolean;
  services: ServiceState[];
  bootstrapError?: string;
}

export interface Trend {
  id: string;
  title: string;
  views: number;
}

export interface NewMovie {
  title: string;
  year: number;
  genre: string;
  director: string;
  synopsis: string;
  rating: number;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });
  const text = await res.text();
  const body = text ? JSON.parse(text) : null;

  if (!res.ok) {
    const detail = body?.detail ? ` (${body.detail})` : "";
    throw new Error(`${body?.error ?? res.statusText}${detail}`);
  }
  return body as T;
}

export const api = {
  search: (q: string, genre: string) => {
    const p = new URLSearchParams();
    if (q) p.set("q", q);
    if (genre) p.set("genre", genre);
    return request<SearchResults>(`/api/movies?${p}`);
  },
  detail: (id: string) => request<MovieDetail>(`/api/movies/${encodeURIComponent(id)}`),
  create: (m: NewMovie) => request<Movie>("/api/movies", { method: "POST", body: JSON.stringify(m) }),
  trending: () => request<{ trending: Trend[] }>("/api/trending"),
  awsStatus: () => request<AwsStatus>("/api/aws/status"),
};
