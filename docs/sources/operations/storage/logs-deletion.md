---
title: Log Entry Deletion
weight: 60
---
# Log Entry Deletion

Grafana Loki supports the deletion of log entries from a specified stream.
Log entries that fall within a specified time window and match an optional line filter are those that will be deleted.

Log entry deletion is supported _only_ when the BoltDB Shipper is configured for the index store.

The compactor component exposes REST [endpoints](../../../api/#compactor) that process delete requests.
Hitting the endpoint specifies the streams and the time window.
The deletion of the log entries takes place after a configurable cancellation time period expires.

Log entry deletion relies on configuration of the custom logs retention workflow as defined for the [compactor](../retention#compactor). The compactor looks at unprocessed requests which are past their cancellation period to decide whether a chunk is to be deleted or not.

## Configuration

Enable log entry deletion by setting `retention_enabled` to true and `deletion_mode` to `filter-only` or `filter-and-delete` in the compactor's configuration.

With `filter-only`, log lines matching the query in the delete request are filtered out when querying Loki. They are not removed from storage.
With `filter-and-delete`, log lines matching the query in the delete request are filtered out when querying Loki, and they are also removed from storage.

A delete request may be canceled within a configurable cancellation period. Set the `delete_request_cancel_period` in the compactor's YAML configuration or on the command line when invoking Loki. Its default value is 24h.

Access to the deletion API can be enabled per tenant via the `allow_deletes` setting.
