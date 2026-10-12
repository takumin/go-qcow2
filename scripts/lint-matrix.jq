{
	include: [
		.modules.reviewdog.recipes | to_entries[]
		| select(.value.attributes | any(.group? == "lint"))
		| {
			name: .key,
			target: .key
		}
	]
}
