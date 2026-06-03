-- Rate-limit rule windows move from nanoseconds to whole seconds in the stored
-- spec JSONB (matching the control-API + ratelimit.Window wire format). Convert
-- each existing rule's window by dividing by 1e9, preserving rule order.
UPDATE rate_limits rl
SET spec = jsonb_set(
        rl.spec,
        '{rules}',
        (
            SELECT jsonb_agg(
                CASE
                    WHEN elem ? 'window'
                    THEN jsonb_set(elem, '{window}',
                                   to_jsonb(((elem->>'window')::bigint) / 1000000000))
                    ELSE elem
                END
                ORDER BY ord
            )
            FROM jsonb_array_elements(rl.spec->'rules') WITH ORDINALITY AS t(elem, ord)
        )
    )
WHERE jsonb_typeof(spec->'rules') = 'array'
  AND jsonb_array_length(spec->'rules') > 0;
