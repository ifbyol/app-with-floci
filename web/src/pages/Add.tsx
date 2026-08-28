import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type Movie } from "../lib/api";

const GENRES = ["Drama", "Thriller", "Comedy", "Documentary", "Romance", "Horror", "Mystery"];

export default function Add() {
  const [title, setTitle] = useState("");
  const [year, setYear] = useState(2026);
  const [genre, setGenre] = useState(GENRES[0]);
  const [director, setDirector] = useState("");
  const [synopsis, setSynopsis] = useState("");
  const [rating, setRating] = useState(7.5);

  const [saved, setSaved] = useState<Movie | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    setSaved(null);
    try {
      const m = await api.create({ title, year, genre, director, synopsis, rating });
      setSaved(m);
      setTitle("");
      setDirector("");
      setSynopsis("");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <h1>Add a movie</h1>
      <p className="lede">
        One request touches all three services: the row is committed to PostgreSQL inside a
        transaction, indexed into OpenSearch, and the cache key is invalidated.
      </p>

      {saved && (
        <div className="notice" style={{ marginBottom: "1rem" }}>
          Saved <strong>{saved.title}</strong>. It is already searchable &mdash;{" "}
          <Link to="/">try finding it</Link> or <Link to={`/movies/${saved.id}`}>open it</Link>.
        </div>
      )}
      {error && (
        <p className="error" style={{ marginBottom: "1rem" }}>
          {error}
        </p>
      )}

      <form className="card" onSubmit={submit}>
        <div className="field">
          <label htmlFor="title">Title</label>
          <input id="title" required value={title} onChange={(e) => setTitle(e.target.value)} />
        </div>

        <div className="two">
          <div className="field">
            <label htmlFor="year">Year</label>
            <input
              id="year"
              type="number"
              min={1880}
              max={2100}
              required
              value={year}
              onChange={(e) => setYear(Number(e.target.value))}
            />
          </div>
          <div className="field">
            <label htmlFor="genre">Genre</label>
            <select id="genre" value={genre} onChange={(e) => setGenre(e.target.value)}>
              {GENRES.map((g) => (
                <option key={g} value={g}>
                  {g}
                </option>
              ))}
            </select>
          </div>
        </div>

        <div className="two">
          <div className="field">
            <label htmlFor="director">Director</label>
            <input id="director" value={director} onChange={(e) => setDirector(e.target.value)} />
          </div>
          <div className="field">
            <label htmlFor="rating">Rating (0&ndash;10)</label>
            <input
              id="rating"
              type="number"
              step={0.1}
              min={0}
              max={10}
              value={rating}
              onChange={(e) => setRating(Number(e.target.value))}
            />
          </div>
        </div>

        <div className="field">
          <label htmlFor="synopsis">Synopsis</label>
          <textarea id="synopsis" value={synopsis} onChange={(e) => setSynopsis(e.target.value)} />
        </div>

        <button type="submit" disabled={busy || !title}>
          {busy ? "Saving…" : "Save movie"}
        </button>
      </form>
    </>
  );
}
