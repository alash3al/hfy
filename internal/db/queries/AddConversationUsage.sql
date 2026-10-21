UPDATE conversations
SET input_tokens = input_tokens + @input,
    output_tokens = output_tokens + @output,
    cached_tokens = cached_tokens + @cached,
    updated_at = now()
WHERE id = @id
