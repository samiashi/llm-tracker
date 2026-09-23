-- +goose Up

-- Remove rows that collectors re-keyed, which would otherwise be counted again
-- beside their replacements. Each rule runs once over what is stored now and
-- again, as a trigger, when a replacement or a straggling old row arrives
-- later: agents upgrade after the server, one at a time.
--
-- Codex (collector 9) keys token_count usage on the usage itself ('tc:...'),
-- not '<rollout>.jsonl#<ordinal>', which gives a restated response and a
-- fork's copy of its parent's lines a new id each. No old row maps onto one
-- new row, so a machine's old rows go when it first reports a 'tc:' key: the
-- upgrade at which its collector re-reads every rollout. Rows keyed on a
-- response id ('resp_...') are untouched.
--
-- Cline, Roo Code (collector 5) and Continue (collectors 5 and 9) let a record
-- vanish from disk, so an old row goes only where the row now keyed on the
-- same record is present; otherwise it is the only one left. The collector
-- applies the same rules to its archive (sources.Superseded).

-- A re-keyed id for a day already rolled up is not in pruned_event, so it
-- would be admitted beside the rollup that counts that day under the old key.
-- 00015's legacy_rollup_floor already covers every rollup day, and Prune
-- raises it while old-key Codex rows remain; the deletes below stop at it.

CREATE TABLE codex_rekeyed_machine (machine_id TEXT PRIMARY KEY);

INSERT INTO codex_rekeyed_machine
  SELECT DISTINCT machine_id FROM event WHERE source = 'codex' AND native_id GLOB 'tc:*';

-- Only from the floor on: a replacement for an earlier day is refused, so
-- deleting the old row there would lose the usage. The pairwise rules below
-- need no floor, since the replacement is already stored. Rows are picked by
-- rowid so the planner looks a machine's up by (source, machine_id) rather
-- than scanning every Codex day.
DELETE FROM event
 WHERE rowid IN (SELECT rowid FROM event
                  WHERE source = 'codex' AND native_id GLOB '*.jsonl#*'
                    AND machine_id IN (SELECT machine_id FROM codex_rekeyed_machine))
   AND day >= (SELECT COALESCE(MAX(value), '') FROM setting WHERE key = 'legacy_rollup_floor');

-- A machine not yet on collector 9 may hold both forms collector 5 left for
-- one line: '<absolute path>#<ordinal>' and '<basename>#<ordinal>'.
DELETE FROM event AS o
 WHERE o.source = 'codex' AND o.native_id GLOB '/*.jsonl#*'
   AND EXISTS (SELECT 1 FROM event AS n
                WHERE n.source = 'codex' AND n.machine_id = o.machine_id AND n.ts = o.ts
                  AND n.native_id GLOB '*.jsonl#*' AND n.native_id NOT GLOB '*/*'
                  AND o.native_id GLOB ('*/' || n.native_id));

-- Cline and Roo Code: '<task>#<array index>' became '<task>#<ms>#<n>'.
DELETE FROM event AS o
 WHERE o.source IN ('cline', 'roo_code')
   AND o.native_id GLOB '*#*' AND o.native_id NOT GLOB '*#*#*'
   AND EXISTS (SELECT 1 FROM event AS n
                WHERE n.source = o.source AND n.machine_id = o.machine_id AND n.ts = o.ts
                  AND n.session_id = o.session_id AND n.native_id GLOB '*#*#*'
                  AND n.input_tokens = o.input_tokens AND n.output_tokens = o.output_tokens
                  AND n.cache_read_tokens = o.cache_read_tokens
                  AND n.cache_write_5m = o.cache_write_5m);

-- Continue: '<absolute path>#<time>#<model>' gives way to any later row for
-- the same record, 'tokensGenerated.jsonl#<offset>' to
-- '<dir>/tokensGenerated.jsonl#<offset>'. Time is not compared for the
-- second: an undated record takes the file's mtime, which moves.
DELETE FROM event AS o
 WHERE o.source = 'continue'
   AND (o.native_id GLOB '/*' OR o.native_id NOT GLOB '*/*')
   AND EXISTS (SELECT 1 FROM event AS n
                WHERE n.source = 'continue' AND n.machine_id = o.machine_id
                  AND n.model = o.model
                  AND n.input_tokens = o.input_tokens AND n.output_tokens = o.output_tokens
                  AND n.native_id NOT GLOB '/*'
                  AND ((o.native_id GLOB '/*' AND n.ts = o.ts)
                    OR (o.native_id NOT GLOB '*/*' AND n.native_id GLOB ('*/' || o.native_id))));

-- +goose StatementBegin
CREATE TRIGGER codex_retire_ordinal_keys AFTER INSERT ON event
WHEN NEW.source = 'codex' AND NEW.native_id GLOB 'tc:*'
 AND NOT EXISTS (SELECT 1 FROM codex_rekeyed_machine WHERE machine_id = NEW.machine_id)
BEGIN
  INSERT INTO codex_rekeyed_machine (machine_id) VALUES (NEW.machine_id);
  DELETE FROM event
   WHERE rowid IN (SELECT rowid FROM event
                    WHERE source = 'codex' AND native_id GLOB '*.jsonl#*'
                      AND machine_id = NEW.machine_id)
     AND day >= (SELECT COALESCE(MAX(value), '') FROM setting WHERE key = 'legacy_rollup_floor');
END;
-- +goose StatementEnd

-- An old-key row from a machine that already reports the new key -- a
-- downgraded agent -- is dropped: the re-read after its next upgrade sends
-- the same usage under the new key.
-- +goose StatementBegin
CREATE TRIGGER codex_refuse_ordinal_keys AFTER INSERT ON event
WHEN NEW.source = 'codex' AND NEW.native_id GLOB '*.jsonl#*'
 AND EXISTS (SELECT 1 FROM codex_rekeyed_machine WHERE machine_id = NEW.machine_id)
BEGIN
  DELETE FROM event WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER cline_retire_index_keys AFTER INSERT ON event
WHEN NEW.source IN ('cline', 'roo_code') AND NEW.native_id GLOB '*#*#*'
BEGIN
  DELETE FROM event
   WHERE source = NEW.source AND machine_id = NEW.machine_id AND ts = NEW.ts
     AND session_id = NEW.session_id
     AND native_id GLOB '*#*' AND native_id NOT GLOB '*#*#*'
     AND input_tokens = NEW.input_tokens AND output_tokens = NEW.output_tokens
     AND cache_read_tokens = NEW.cache_read_tokens AND cache_write_5m = NEW.cache_write_5m;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER cline_refuse_index_keys AFTER INSERT ON event
WHEN NEW.source IN ('cline', 'roo_code')
 AND NEW.native_id GLOB '*#*' AND NEW.native_id NOT GLOB '*#*#*'
 AND EXISTS (SELECT 1 FROM event
              WHERE source = NEW.source AND machine_id = NEW.machine_id AND ts = NEW.ts
                AND session_id = NEW.session_id AND native_id GLOB '*#*#*'
                AND input_tokens = NEW.input_tokens AND output_tokens = NEW.output_tokens
                AND cache_read_tokens = NEW.cache_read_tokens
                AND cache_write_5m = NEW.cache_write_5m)
BEGIN
  DELETE FROM event WHERE id = NEW.id;
END;
-- +goose StatementEnd

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

-- +goose Down

-- Deleted rows are not restored, and the floor is not lowered: the rows
-- counted usage their replacements now hold, and restoring either would count
-- it twice.
DROP TRIGGER IF EXISTS continue_refuse_old_keys;
DROP TRIGGER IF EXISTS continue_retire_old_keys;
DROP TRIGGER IF EXISTS cline_refuse_index_keys;
DROP TRIGGER IF EXISTS cline_retire_index_keys;
DROP TRIGGER IF EXISTS codex_refuse_ordinal_keys;
DROP TRIGGER IF EXISTS codex_retire_ordinal_keys;
DROP TABLE IF EXISTS codex_rekeyed_machine;
