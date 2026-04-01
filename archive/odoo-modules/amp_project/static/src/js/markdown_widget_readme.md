# AMP Markdown Widget

A rich markdown editing widget for Odoo forms that provides live preview, syntax highlighting, and an intuitive toolbar.

## Features

- **Live Preview**: Real-time markdown rendering as you type
- **Three View Modes**: 
  - Edit: Full editor with syntax highlighting
  - Preview: Rendered markdown view
  - Split: Side-by-side editor and preview
- **Toolbar**: Quick formatting buttons for common markdown elements
- **Keyboard Shortcuts**: 
  - Ctrl+B: Bold
  - Ctrl+I: Italic  
  - Ctrl+K: Link
- **Responsive Design**: Works on desktop and mobile devices
- **Odoo Integration**: Seamless integration with Odoo's field framework

## Usage

### In XML Views

```xml
<field name="your_text_field" widget="markdown"/>
```

### Supported Field Types

- `fields.Text`
- `fields.Html` (content will be treated as markdown)

### Example Implementation

```xml
<page string="Description">
    <field name="description" widget="markdown"/>
</page>
```

## Markdown Syntax Supported

### Headers
```markdown
# Header 1
## Header 2  
### Header 3
```

### Text Formatting
```markdown
**Bold text**
*Italic text*
***Bold and italic***
```

### Code
```markdown
`inline code`

```
code block
```
```

### Links
```markdown
[Link text](https://example.com)
```

### Lists
```markdown
- List item 1
- List item 2
* Alternative bullet
```

## Toolbar Buttons

| Button | Function | Shortcut |
|--------|----------|----------|
| ✏️ | Edit mode | - |
| ⚌ | Split mode | - |
| 👁 | Preview mode | - |
| **B** | Bold | Ctrl+B |
| *I* | Italic | Ctrl+I |
| `</>` | Inline code | - |
| `{ }` | Code block | - |
| H1 | Header 1 | - |
| H2 | Header 2 | - |
| H3 | Header 3 | - |
| 🔗 | Link | Ctrl+K |
| • List | Bullet list | - |

## Technical Details

### Architecture

- **Component**: OWL component extending Odoo's field framework
- **Parser**: Custom markdown parser with HTML output
- **Highlighter**: Basic syntax highlighting for editor
- **Integration**: Uses `useInputField` hook for proper field binding

### Files

- `markdown_widget.js` - Main widget implementation
- `markdown_widget.xml` - OWL template
- `markdown_widget.css` - Styling and responsive design

### Performance

- Efficient parsing with minimal DOM manipulation
- Debounced preview updates for large content
- Proper event cleanup to prevent memory leaks

## Customization

### Extending the Parser

To add new markdown syntax support, extend the `MarkdownParser` class:

```javascript
class CustomMarkdownParser extends MarkdownParser {
    static parse(markdown) {
        let html = super.parse(markdown);
        // Add custom parsing rules
        html = html.replace(/~~(.*?)~~/gim, '<del>$1</del>'); // Strikethrough
        return html;
    }
}
```

### Custom Styling

Override CSS classes to customize appearance:

```css
.amp-markdown-widget {
    /* Custom widget styling */
}

.amp-markdown-preview h1 {
    /* Custom header styling */
}
```

## Browser Support

- Chrome 80+
- Firefox 75+
- Safari 13+
- Edge 80+

## Known Limitations

1. Basic syntax highlighting (not full-featured like CodeMirror)
2. Simple markdown parser (doesn't support all CommonMark features)
3. No table support in current version
4. No image upload integration

## Future Enhancements

- [ ] Advanced syntax highlighting with CodeMirror
- [ ] Full CommonMark compliance
- [ ] Table editing support
- [ ] Image upload and embedding
- [ ] Math formula support (KaTeX)
- [ ] Export to PDF/HTML
- [ ] Collaborative editing
- [ ] Plugin system for extensions