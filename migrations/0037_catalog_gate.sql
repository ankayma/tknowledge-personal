-- The public catalog admits an instruction only when it ran active in a real
-- store, carries a dated incident, has the consent of the person who signs
-- that store and holds nothing company-specific (owner 14/10/2026, A-157).
-- The approval is where the maintainer attests the last two and names the
-- store without naming the person; the incident's date goes with it.
ALTER TABLE catalog_review ADD COLUMN IF NOT EXISTS source text;
ALTER TABLE catalog_review ADD COLUMN IF NOT EXISTS incident_on date;
