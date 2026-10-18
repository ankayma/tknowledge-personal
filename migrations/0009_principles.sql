-- The Principle List is its own trust class (P.9): only principles and sourced facts are
-- trusted without checking. Each principle is a subject chain principle:<code>.statement
-- (the canonical sentence, T) and principle:<code>.gloss (the [A] part, if any).
INSERT INTO namespaces (name, description) VALUES
  ('principle', 'nguyên tắc nền, hạng tin cậy riêng (P.9)')
ON CONFLICT (name) DO NOTHING;
