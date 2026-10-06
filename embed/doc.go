// Package embed is the client of the playground's local embeddings service (Hugging Face text-embeddings-inference,
// BAAI/bge-base-en-v1.5 by default: 768 dimensions, English, no key, no cost) and the pgvector helpers an app needs.
//
// The platform injects EMBED_URL, EMBED_MODEL and EMBED_DIMENSIONS into the containers of an app that opted in
// (`playground embeddings <app> on`); FromEnv reads them:
//
//	emb, err := embed.FromEnv()
//	vecs, err := emb.Embed(ctx, []string{"a note", "another"}, embed.Passage) // store these
//	q, err := emb.Embed(ctx, []string{"what did I write about X"}, embed.Query) // search with this
//	rows, err := pool.Query(ctx, `SELECT id FROM notes ORDER BY embedding <=> $1 LIMIT 10`, embed.Vector(q[0]))
//
// Use Passage for the text you store and Query for the text a user types: bge (and e5) models are trained with different
// prefixes for the two, and the client applies the right one for the configured model. Tests use Fake, which needs no service.
//
// Endpoints relied on (text-embeddings-inference): POST /embed with {"inputs":[...],"normalize":true,"truncate":true},
// answering a JSON array of float arrays in input order; GET /health for Health. Errors come back as
// {"error":"...","error_type":"..."} with 413 (batch or payload too large), 422 (invalid input), 424 (inference failed),
// 429 (overloaded) and 503; 429, 502, 503, 504, 424 and network errors are retried with backoff, the others are not.
package embed
