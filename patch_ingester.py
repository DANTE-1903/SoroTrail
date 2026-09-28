with open("internal/ingester/ingester.go", "r") as f:
    content = f.read()

target = "// skipContracts is the denylist map built from opts.SkipContracts for O(1) filtering."
replacement = "// skipContracts is the denylist map built from opts.SkipContracts for O(1) filtering.\n\t// Events originating from these contracts will be dropped silently before persistence."

content = content.replace(target, replacement)

with open("internal/ingester/ingester.go", "w") as f:
    f.write(content)
