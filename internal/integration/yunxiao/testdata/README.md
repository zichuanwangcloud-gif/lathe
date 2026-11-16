本目录的 fixture 按阿里云云效官方 OpenAPI 文档的响应形状手写（取证日期 2025-11，
见 docs/features/tracker-yunxiao.md §2.6 与 §6 R1），不是真实抓包：

- get_workitem.json —— GetWorkitem
  https://www.alibabacloud.com/help/zh/yunxiao/developer-reference/getworkitem

文档示例值多为占位；解析只依赖 §2.6 表中列出的字段，其余字段忽略。
