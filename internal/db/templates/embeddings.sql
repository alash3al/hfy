CREATE TABLE IF NOT EXISTS {{.Table}} (
    id              bigserial PRIMARY KEY,
    embeddable_type text        NOT NULL,
    embeddable_id   bigint      NOT NULL,
    scope           text,
    model           text        NOT NULL,
    embedding       vector({{.Dims}}) NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS {{.Table}}_uniq ON {{.Table}} (embeddable_type, embeddable_id, model);
CREATE INDEX IF NOT EXISTS {{.Table}}_filter ON {{.Table}} (embeddable_type, scope, model);
{{if .Half -}}
CREATE INDEX IF NOT EXISTS {{.Table}}_hnsw ON {{.Table}} USING hnsw ((embedding::halfvec({{.Dims}})) halfvec_cosine_ops);
{{- else -}}
CREATE INDEX IF NOT EXISTS {{.Table}}_hnsw ON {{.Table}} USING hnsw (embedding vector_cosine_ops);
{{- end}}
