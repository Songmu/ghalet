# Repository guidance

## Package organization

Use ordinary subpackages rather than `internal/`. This is a CLI application;
external consumers of its subpackages are responsible for compatibility.

## Documentation

Keep English and Japanese user documentation synchronized in the same change.
This includes `README.md` / `README.ja.md` and `docs/*.md` / `docs/*.ja.md`.
When adding user documentation, provide both language versions and their language
switcher links. Keep cross-references consistent with each document's language.
