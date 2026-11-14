-- T47：组件 Wappalyzer 分类。只加列；分类名回填由启动/迁移后的本地指纹库完成，不对外请求、不重扫。
ALTER TABLE components ADD COLUMN IF NOT EXISTS categories TEXT[] NOT NULL DEFAULT '{}';
