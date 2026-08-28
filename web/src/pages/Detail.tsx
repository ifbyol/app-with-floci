import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, type MovieDetail } from "../lib/api";

export default function Detail() {
  const { id = "" } = useParams();
  const [data, setData] = useState<MovieDetail | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    setBusy(true);
    api
      .detail(id)
      .then((d) => {
        setData(d);
        setError("");
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  }, [id]);

  useEffect(load, [load]);

  if (error) {
    return (
      <>
        <p className="error">{error}</p>
        <Link to="/">&larr; Back to search</Link>
      </>
    );
  }
  if (!data) return <p className="notice">Loading&hellip;</p>;

  const { movie, cache } = data;
  const ms = (cache.lookupMicros / 1000).toFixed(2);

  return (
    <>
      <Link to="/" className="meta">
        &larr; Back to search
      </Link>
      <h1 style={{ marginTop: ".75rem" }}>{movie.title}</h1>
      <p className="lede">
        {movie.year} &middot; {movie.genre} &middot; dir. {movie.director} &middot; {movie.rating.toFixed(1)}/10
      </p>

      <div className="card" style={{ marginBottom: "1.25rem" }}>
        <div className="row" style={{ justifyContent: "space-between" }}>
          <span className={`badge ${cache.hit ? "hit" : "miss"}`}>
            cache {cache.hit ? "hit" : "miss"}
          </span>
          <span className="meta">
            served from <strong>{cache.source}</strong> in {ms} ms
          </span>
        </div>
        <p className="svc-note">
          {cache.hit
            ? `Read straight from Valkey through Floci's ElastiCache proxy. The key expires after ${cache.ttlSeconds}s.`
            : `Key was cold, so this came from PostgreSQL through Floci's RDS proxy and has now been cached for ${cache.ttlSeconds}s. Read it again to see the difference.`}
        </p>
        <div className="row" style={{ marginTop: ".75rem" }}>
          <button onClick={load} disabled={busy}>
            {busy ? "Reading…" : "Read again"}
          </button>
        </div>
      </div>

      {movie.synopsis && <p>{movie.synopsis}</p>}

      <h2>Record</h2>
      <div className="card">
        <dl className="kv">
          <dt>id</dt>
          <dd className="mono">{movie.id}</dd>
          <dt>created</dt>
          <dd className="mono">{new Date(movie.createdAt).toLocaleString()}</dd>
        </dl>
      </div>
    </>
  );
}
