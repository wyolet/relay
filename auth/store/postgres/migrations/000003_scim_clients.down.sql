-- Drops what 000003 added; every SCIM client's token stops working. The rows the clients provisioned keep their external_source.
DROP TABLE IF EXISTS scim_clients;
