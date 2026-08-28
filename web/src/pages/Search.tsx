import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type SearchResults, type Trend } from "../lib/api";
import { useDebounced } from "../lib/useDebounced";

export default function Search() {
  const [query, setQuery] = useState("");
  const [genre, setGenre] = useState("");
  const [results, setResults] = useState<SearchResults | null>(null);
  const [trending, setTrending] = useState<Trend[]>([]);
  const [error, setError] = useState("");

  const debouncedQuery = useDebounced(query);

  useEffect(() => {
    let alive = true;
    api
      .search(debouncedQuery, genre)
      .then((r) => alive && (setResults(r), setError("")))
      .catch((e: Error) => alive && setError(e.message));
    return () => {
      alive = false;
    };
  }, [debouncedQuery, genre]);

  useEffect(() => {
    api
      .trending()
      .then((r) => setTrending(r.trending))
      .catch(() => setTrending([]));
  }, []);

  return (
    <>
      <h1>Search the catalogue</h1>
      <p className="lede">
        Full-text and fuzzy matching served by OpenSearch. Try a typo &mdash;{" "}
        <code>nemspace</code> still finds <em>Namespace</em>.
      </p>

      <div className="row" style={{ marginBottom: "1rem" }}>
        <input
          type="search"
          placeholder="Title, director or synopsis..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="Search movies"
        />
      </div>

      {results && results.genres.length > 0 && (
        <div className="row" style={{ marginBottom: "1.25rem" }}>
          <button className={`chip ${genre === "" ? "on" : ""}`} onClick={() => setGenre("")}>
            All
          </button>
          {results.genres.map((g) => (
            <button
              key={g.key}
              className={`chip ${genre === g.key ? "on" : ""}`}
              onClick={() => setGenre(genre === g.key ? "" : g.key)}
            >
              {g.key} <span className="n">{g.count}</span>
            </button>
          ))}
        </div>
      )}

      {error && <p className="error">{error}</p>}

      {results && (
        <>
          <p className="meta">
            {results.hits.length < results.total
              ? `showing ${results.hits.length} of ${results.total} results`
              : `${results.total} ${results.total === 1 ? "result" : "results"}`}{" "}
            in {results.tookMillis} ms
          </p>
          <div className="grid">
            {results.hits.map((h) => (
              <article className="card movie" key={h.id}>
                <div>
                  <Link to={`/movies/${h.id}`}>{h.title}</Link>
                  <div className="meta">
                    {h.year} &middot; {h.genre} &middot; dir. {h.director} &middot; {h.rating.toFixed(1)}/10
                  </div>
                  {h.synopsis && <p className="synopsis">{h.synopsis}</p>}
                </div>
                <span className="score">score {h.score.toFixed(2)}</span>
              </article>
            ))}
          </div>
          {results.hits.length === 0 && <p className="notice">Nothing matched. Try a shorter query.</p>}
        </>
      )}

      {trending.length > 0 && (
        <>
          <h2>Most viewed</h2>
          <p className="lede" style={{ marginBottom: ".75rem" }}>
            View counters kept in a Valkey sorted set, incremented on every detail read.
          </p>
          <div className="card">
            {trending.map((t) => (
              <div className="trend" key={t.id}>
                <Link to={`/movies/${t.id}`} style={{ color: "inherit" }}>
                  {t.title || t.id}
                </Link>
                <span className="n">{t.views}</span>
              </div>
            ))}
          </div>
        </>
      )}
    </>
  );
}
