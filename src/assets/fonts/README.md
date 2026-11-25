# 自托管字体（P2-26）

运行期不再向 `fonts.googleapis.com` / `fonts.gstatic.com` 发起请求：`src/styles/fonts.css`
通过 `@font-face` 引用本目录的 woff2，Vite 构建时会把字体作为带指纹的静态资源输出。

## 文件与来源

| 文件 | 内容 | 体积 | 上游 |
| --- | --- | --- | --- |
| `geist-*.woff2` | Geist variable（字重 400–700）latin / latin-ext / cyrillic / cyrillic-ext / vietnamese 子集 | 76.4 KB | [Google Fonts: Geist](https://fonts.google.com/specimen/Geist)，[vercel/geist-font](https://github.com/vercel/geist-font) |
| `jetbrains-mono-*.woff2` | JetBrains Mono variable（字重 400–600）latin / latin-ext / cyrillic / cyrillic-ext / greek / vietnamese 子集 | 66.3 KB | [Google Fonts: JetBrains Mono](https://fonts.google.com/specimen/JetBrains+Mono)，[JetBrains/JetBrainsMono](https://github.com/JetBrains/JetBrainsMono) |
| `material-symbols-outlined-subset.woff2` | Material Symbols Outlined variable（`FILL`/`GRAD`/`opsz`/`wght` 轴），按本仓库实际用到的图标裁剪后的子集 | 40.2 KB | [Google Fonts: Material Symbols](https://fonts.google.com/icons)，[google/material-design-icons](https://github.com/google/material-design-icons) |

上游字体文件由 Google Fonts CSS API（现代浏览器 UA）解析得到 woff2 直链后直接下载，
未做任何字形修改；Geist 与 JetBrains Mono 保留上游发布的 subset 与 `unicode-range`。
Material Symbols 只做子集裁剪（见下节），保留 variable 轴与 `rlig` 连字。

## Material Symbols 子集

上游 variable font 含 4000+ 图标、约 4 MB，对首屏不可接受。因此只保留：
基础 ASCII（连字输入需要）、`src/` 中出现过的图标字形及其连字规则、TrueType 基础字形。
当前覆盖 44 个图标（`add`、`alt_route`、`build`、`construction`、`delete`、`description`、
`dns`、`domain`、`done`、`error`、`fact_check`、`folder_open`、`how_to_reg`、`http`、
`inbox`、`key`、`keyboard_return`、`lock`、`logout`、`menu`、`menu_book`、`monitor_heart`、
`password`、`pending`、`person`、`person_add`、`play_arrow`、`policy`、`public`、`remove`、
`restart_alt`、`rocket_launch`、`rule`、`search`、`security`、`settings`、`shield`、
`shield_lock`、`stop_circle`、`target`、`terminal`、`visibility`、`visibility_off`、`warning`）。

新增图标后需要重新裁剪；命令（fonttools 4.68、brotli 1.2）：

```powershell
# 1. 用 src/ 中的字符串字面量求交集得到图标字形清单（每行一个 glyph 名）
# 2. 不带 layout closure 做子集，否则所有连字输出都会被 ASCII 输入“拉”回来
python -m fontTools.subset material-symbols-outlined.woff2 `
  --unicodes=U+0020-007E `
  --glyphs-file=ms-subset-glyphs.txt `
  --layout-features='*' --no-layout-closure `
  --flavor=woff2 --glyph-names `
  --output-file=material-symbols-outlined-subset.woff2
```

上游文件可从
`https://fonts.googleapis.com/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@20..48,100..700,0..1,-50..200&display=block`
（现代浏览器 UA）解析出 woff2 直链后下载。

## 许可证

- Geist：SIL Open Font License 1.1，见 `Geist-OFL.txt`。
- JetBrains Mono：SIL Open Font License 1.1，见 `JetBrainsMono-OFL.txt`。
- Material Symbols：Apache License 2.0，见 `MaterialSymbols-LICENSE.txt`。
