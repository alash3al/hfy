INSERT INTO {{table}} (embeddable_type, embeddable_id, scope, model, embedding)
VALUES ('conversation_event', @id, @scope, @model, @embedding::vector)
ON CONFLICT (embeddable_type, embeddable_id, model) DO NOTHING
