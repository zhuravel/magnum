{{template "task" .}} Your role: test each point of the ticket against the code of this copy.

{{template "inputs" .}}

Split the ticket into its points, each thing it asks for. For each point: what exists already (file:line), what is missing, and which files change. Collect the product questions the ticket leaves open: a behaviour it does not decide, a case it does not name; read {{.TaskDir}}/questions.json first when it is there, and leave out what it asks already.

Write {{.Findings}}: one section per point, with what exists, what is missing and the files that change; then `## Open questions`, each with why it matters and the options you see.

{{template "rules" .}}
