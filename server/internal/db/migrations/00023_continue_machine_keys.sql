-- +goose Up

-- Continue (collector 10) keys a record on '<machine>:<path under
-- dev_data>#<offset>'. Its earlier keys carry no machine, and an event's id is
-- derived from its key, so two colleagues' records at the same offset of the
-- same file -- every file starts at 0 -- were merged into one row. By collector:
--   before 5   '/<absolute path>#<time>#<model>'
--   5 to 8     '<basename>#<offset>'
--   9          '<path under dev_data>#<offset>', with no '/' for a root file
--   10         '<machine>:<path under dev_data>#<offset>'
-- A collector-9 row R gives way only to its own machine's 'M:R'. A 5-to-8 row
-- R gives way to a row ending in '/R' or ':R' with the same model and tokens,
-- time uncompared (an undated record takes the file's mtime, which moves),
-- and never to a collector-9 row that merely shares its shape: a root file's
-- current key. A path-keyed row keeps 00016's rule. Each rule matches on the
-- collector as well as the shape, and the agent applies the same pairing to
-- its archive (sources.Superseded).

DROP TRIGGER continue_retire_old_keys;
DROP TRIGGER continue_refuse_old_keys;

-- Pairwise, so no floor: the replacement is already stored.
DELETE FROM event AS o
 WHERE o.source = 'continue'
   AND EXISTS (SELECT 1 FROM event AS n
                WHERE n.source = 'continue' AND n.machine_id = o.machine_id AND n.id != o.id
                  AND ((o.native_id GLOB '/*' AND n.native_id NOT GLOB '/*' AND n.ts = o.ts
                        AND n.model = o.model AND n.input_tokens = o.input_tokens
                        AND n.output_tokens = o.output_tokens)
                    OR (o.collector < 9 AND instr(o.native_id, '/') = 0
                        AND substr(n.native_id, -length(o.native_id) - 1)
                            IN ('/' || o.native_id, ':' || o.native_id)
                        AND n.model = o.model AND n.input_tokens = o.input_tokens
                        AND n.output_tokens = o.output_tokens)
                    OR (o.collector = 9 AND n.collector >= 10 AND instr(n.native_id, ':') > 0
                        AND substr(n.native_id, instr(n.native_id, ':') + 1) = o.native_id)));

-- +goose StatementBegin
CREATE TRIGGER continue_retire_superseded_keys AFTER INSERT ON event
WHEN NEW.source = 'continue' AND NEW.native_id NOT GLOB '/*'
BEGIN
  DELETE FROM event
   WHERE source = 'continue' AND machine_id = NEW.machine_id AND id != NEW.id
     AND ((native_id GLOB '/*' AND ts = NEW.ts AND model = NEW.model
           AND input_tokens = NEW.input_tokens AND output_tokens = NEW.output_tokens)
       OR (collector < 9 AND instr(native_id, '/') = 0
           AND substr(NEW.native_id, -length(native_id) - 1) IN ('/' || native_id, ':' || native_id)
           AND model = NEW.model
           AND input_tokens = NEW.input_tokens AND output_tokens = NEW.output_tokens)
       OR (collector = 9 AND NEW.collector >= 10 AND instr(NEW.native_id, ':') > 0
           AND native_id = substr(NEW.native_id, instr(NEW.native_id, ':') + 1)));
END;
-- +goose StatementEnd

-- An old-key row arriving after its replacement -- a downgraded agent, or an
-- archive replayed -- is dropped as it lands.
-- +goose StatementBegin
CREATE TRIGGER continue_refuse_superseded_keys AFTER INSERT ON event
WHEN NEW.source = 'continue' AND NEW.collector < 10
 AND EXISTS (SELECT 1 FROM event AS n
              WHERE n.source = 'continue' AND n.machine_id = NEW.machine_id AND n.id != NEW.id
                AND ((NEW.native_id GLOB '/*' AND n.native_id NOT GLOB '/*' AND n.ts = NEW.ts
                      AND n.model = NEW.model AND n.input_tokens = NEW.input_tokens
                      AND n.output_tokens = NEW.output_tokens)
                  OR (NEW.collector < 9 AND instr(NEW.native_id, '/') = 0
                      AND substr(n.native_id, -length(NEW.native_id) - 1)
                          IN ('/' || NEW.native_id, ':' || NEW.native_id)
                      AND n.model = NEW.model AND n.input_tokens = NEW.input_tokens
                      AND n.output_tokens = NEW.output_tokens)
                  OR (NEW.collector = 9 AND n.collector >= 10 AND instr(n.native_id, ':') > 0
                      AND substr(n.native_id, instr(n.native_id, ':') + 1) = NEW.native_id)))
BEGIN
  DELETE FROM event WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down

-- Deleted rows are not restored: their replacements hold the same usage.
DROP TRIGGER IF EXISTS continue_refuse_superseded_keys;
DROP TRIGGER IF EXISTS continue_retire_superseded_keys;

-- 00016's rules, as it created them.
-- +goose StatementBegin
CREATE TRIGGER continue_retire_old_keys AFTER INSERT ON event
WHEN NEW.source = 'continue' AND NEW.native_id NOT GLOB '/*'
BEGIN
  DELETE FROM event
   WHERE source = 'continue' AND machine_id = NEW.machine_id AND id != NEW.id
     AND model = NEW.model
     AND input_tokens = NEW.input_tokens AND output_tokens = NEW.output_tokens
     AND ((native_id GLOB '/*' AND ts = NEW.ts)
       OR (native_id NOT GLOB '*/*' AND NEW.native_id GLOB ('*/' || native_id)));
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER continue_refuse_old_keys AFTER INSERT ON event
WHEN NEW.source = 'continue'
 AND (NEW.native_id GLOB '/*' OR NEW.native_id NOT GLOB '*/*')
 AND EXISTS (SELECT 1 FROM event
              WHERE source = 'continue' AND machine_id = NEW.machine_id AND id != NEW.id
                AND model = NEW.model
                AND input_tokens = NEW.input_tokens AND output_tokens = NEW.output_tokens
                AND native_id NOT GLOB '/*'
                AND ((NEW.native_id GLOB '/*' AND ts = NEW.ts)
                  OR (NEW.native_id NOT GLOB '*/*' AND native_id GLOB ('*/' || NEW.native_id))))
BEGIN
  DELETE FROM event WHERE id = NEW.id;
END;
-- +goose StatementEnd
