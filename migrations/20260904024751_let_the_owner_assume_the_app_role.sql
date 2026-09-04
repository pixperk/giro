-- let_the_owner_assume_the_app_role
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- "set role giro_app" is how the suite runs itself as the application role and
-- how an operator checks what that role can actually reach. it works only for
-- a superuser or for a member of the role, and neither is something a
-- migration should assume: it happens to hold on a development machine and in
-- a container whose bootstrap user is a superuser, and stops holding the first
-- time the schema is owned by an ordinary role.
--
-- so the role that migrates may take the application role on. this grants
-- nothing: the owner already outranks giro_app in every respect, and this only
-- lets it step down to look.
--
-- deliberately not the reverse. giro_app is a member of nothing, so there is
-- no path back up from it, which is the property that makes "reset role"
-- useless to anything holding it.
do $$
begin
    execute format('grant giro_app to %I', current_user);
end
$$;
