ALTER TABLE reports 
ALTER COLUMN access_token 
SET DEFAULT replace(replace(replace(
    encode(gen_random_bytes(24), 'base64'),
    '+', '-'),
    '/', '_'),
    '=', '');