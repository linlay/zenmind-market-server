# Version-level unpublish and administrator management

## Goal

Make administrator management focus on publication review and lifecycle control. An administrator can unpublish only the current latest version of a component. The marketplace then automatically serves the highest remaining approved and published version.

## Scope

- Add version-level unpublish semantics that safely fall back from the latest version to a remaining public version.
- Ensure catalog, resolve, download, and ADP manifest requests cannot expose an unpublished version.
- Replace administrator dashboard analytics with moderation and component-management views.
- Preserve the creator dashboard and its personal analytics unchanged.

## Server behaviour

`POST /api/v1/admin/unpublish` continues to accept `type`, `id`, and `version`.

The administrator UI will always supply the component's current `latestVersion`; the server also validates that the requested version is the current latest version. Requests to unpublish a historical version are rejected.

In one SQLite transaction, the server will:

1. Mark the selected version and all of its platforms unpublished.
2. Find the highest semantic version for the same component that is approved and published.
3. If a replacement exists, update `items.latest_version` and refresh the item-level metadata, review state, and publication state from that replacement version.
4. If no replacement exists, mark the item unpublished so it disappears from the public catalog.

The server will use a shared SemVer comparison helper rather than publication time or lexical text ordering. Existing malformed historical versions are not selected automatically; publishing validation is otherwise unchanged in this scope.

Artifact resolution and downloads will join against an approved, published version and item. Direct URLs through `/download`, the resolve endpoint, and ADP manifests therefore cannot reach a version that was unpublished.

## Administrator UI

Administrator mode will have two management sections:

- **Publication review:** pending submissions, details, approve, and reject.
- **Published components:** the public component inventory, filtering/searching, version history, and a destructive-action-confirmed "Unpublish latest version" control.

The administrator view will not render creator metrics, quality checks, recent updates, download charts, or favorite charts. Those remain exclusively in creator mode.

The unpublish action includes the selected component name and version in its confirmation prompt. On success the page reloads review and published component data so the replacement version appears immediately; if no replacement exists, the component disappears from the published list.

## Error handling

- Non-admin requests remain forbidden.
- A component or version that does not exist returns not found.
- Attempting to unpublish a version other than the current latest returns a validation error.
- Failure to find a usable artifact does not expose the unpublished artifact; resolve returns no artifact and download returns not found.
- The UI shows the API error and keeps the list unchanged on failure.

## Tests

Server tests will prove that:

1. Unpublishing latest version falls back to the highest remaining approved version.
2. The fallback applies to catalog, resolve/download, and ADP lookup.
3. With no replacement, the item is absent from the public catalog and cannot download.
4. A historical-version unpublish request is rejected.

Website verification will ensure administrator mode does not render analytics and sends the current latest version to the unpublish endpoint after confirmation.

## Out of scope

- User-facing historical-version downloads and rollback controls.
- Delete of artifact files from persistent storage.
- New update-check APIs or Desktop-side atomic installation.
- Changes to the creator dashboard.
