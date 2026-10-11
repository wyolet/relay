-- Drops the directory tables and their rows. It fails, and drops nothing, while another table still references users, service_accounts or projects, or another trigger still uses bump_resource_version: those belong to a product that shares the tables.
DROP TABLE IF EXISTS role_binding_subjects, role_bindings, roles, group_members, groups, service_accounts, projects, teams, users;
DROP FUNCTION IF EXISTS bump_resource_version();
