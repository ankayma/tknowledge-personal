-- tk_checker must be able to READ flags: the sweep deduplicates (flags.Exists) and closes
-- flags per claim (flags.ForClaim) before INSERT/UPDATE. 0001 only granted INSERT, UPDATE,
-- so the tk-checker container crash-looped with 42501. Tests did not catch it because
-- they ran as superuser; the internal/checker test now connects as the tk_checker role.
GRANT SELECT ON flags TO tk_checker;
