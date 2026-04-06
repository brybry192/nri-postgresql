-- Create a dedicated replication user for the streaming replica.
-- The replica connects with this user via primary_conninfo.
CREATE USER repl WITH REPLICATION ENCRYPTED PASSWORD 'repl_password';

-- Allow the replication user to connect from any host inside the Docker network.
-- pg_hba.conf entries added via ALTER SYSTEM are not supported, so we rely on
-- the default trust/md5 rules. The REPLICATION privilege alone is sufficient.
