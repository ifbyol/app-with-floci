// Package model holds the types shared by the storage, search and cache layers.
package model

import "time"

// Movie is the catalogue record. PostgreSQL is the source of truth; the same
// shape is denormalised into OpenSearch for querying and cached in Valkey.
type Movie struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Year      int       `json:"year"`
	Genre     string    `json:"genre"`
	Director  string    `json:"director"`
	Synopsis  string    `json:"synopsis"`
	Rating    float64   `json:"rating"`
	CreatedAt time.Time `json:"createdAt"`
}
