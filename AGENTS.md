# Repository guidance

## Package organization

Use ordinary subpackages rather than `internal/`. This is a CLI application;
external consumers of its subpackages are responsible for compatibility.

## Documentation

Keep `README.md` / `README.ja.md` and `docs/` focused on ghalet users.
Put contributor instructions in `CONTRIBUTING.md` / `CONTRIBUTING.ja.md` and
`contributing/`; keep package-specific documentation beside its code.
Keep site build tools and their maintenance guide colocated in `docs/site/`
as an exception; they are not published as site pages.
See [the documentation policy](CONTRIBUTING.md#documentation-policy) for the
audience boundary and placement rules.

Keep English and Japanese documentation synchronized in the same change,
including contributor documentation. Provide both language versions and their
language switcher links. Keep cross-references consistent with each document's
language.
