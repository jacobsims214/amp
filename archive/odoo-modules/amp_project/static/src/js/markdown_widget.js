/** @odoo-module **/
/**
 * AMP Markdown Widget — Field widget for rich markdown editing
 * Provides live preview, syntax highlighting, and editor toolbar
 */

import { Component, useState, onMounted, onWillUnmount, useRef } from "@odoo/owl";
import { registry } from "@web/core/registry";
import { standardFieldProps } from "@web/views/fields/standard_field_props";
import { useInputField } from "@web/views/fields/input_field_hook";

// ── Markdown Parser (Simple implementation) ──────────────────────────────────

class MarkdownParser {
    static parse(markdown) {
        if (!markdown) return '';
        
        let html = markdown
            // Headers
            .replace(/^### (.*$)/gim, '<h3>$1</h3>')
            .replace(/^## (.*$)/gim, '<h2>$1</h2>')
            .replace(/^# (.*$)/gim, '<h1>$1</h1>')
            
            // Bold and Italic
            .replace(/\*\*\*(.*?)\*\*\*/gim, '<strong><em>$1</em></strong>')
            .replace(/\*\*(.*?)\*\*/gim, '<strong>$1</strong>')
            .replace(/\*(.*?)\*/gim, '<em>$1</em>')
            
            // Code blocks
            .replace(/```([\s\S]*?)```/gim, '<pre><code>$1</code></pre>')
            .replace(/`([^`]+)`/gim, '<code>$1</code>')
            
            // Links
            .replace(/\[([^\]]+)\]\(([^)]+)\)/gim, '<a href="$2" target="_blank">$1</a>')
            
            // Lists
            .replace(/^\* (.+$)/gim, '<li>$1</li>')
            .replace(/^- (.+$)/gim, '<li>$1</li>')
            
            // Line breaks
            .replace(/\n\n/gim, '</p><p>')
            .replace(/\n/gim, '<br>');
            
        // Wrap in paragraphs and fix lists
        html = '<p>' + html + '</p>';
        html = html.replace(/<p><li>/gim, '<ul><li>');
        html = html.replace(/<\/li><\/p>/gim, '</li></ul>');
        html = html.replace(/<\/li><li>/gim, '</li><li>');
        
        return html;
    }
}

// ── Syntax Highlighter ───────────────────────────────────────────────────────

class MarkdownHighlighter {
    static highlight(textarea) {
        // Simple syntax highlighting by adding CSS classes
        // This would be enhanced with a proper syntax highlighter in production
        const value = textarea.value;
        const highlightedValue = value
            .replace(/(^#{1,6}\s.*$)/gm, '<span class="md-header">$1</span>')
            .replace(/(\*\*.*?\*\*)/g, '<span class="md-bold">$1</span>')
            .replace(/(\*.*?\*)/g, '<span class="md-italic">$1</span>')
            .replace(/(`.*?`)/g, '<span class="md-code">$1</span>')
            .replace(/(```[\s\S]*?```)/g, '<span class="md-code-block">$1</span>');
            
        return highlightedValue;
    }
}

// ── Main Markdown Widget Component ───────────────────────────────────────────

class MarkdownWidget extends Component {
    static template = "amp_project.MarkdownWidget";
    static props = {
        ...standardFieldProps,
    };

    setup() {
        this.textareaRef = useRef("textarea");
        this.previewRef = useRef("preview");
        
        this.state = useState({
            mode: 'split', // 'edit', 'preview', 'split'
            value: this.props.record.data[this.props.name] || '',
            previewHtml: '',
        });

        // Use Odoo's input field hook for proper field integration
        useInputField({
            getValue: () => this.state.value,
            refName: "textarea",
            parse: (value) => value,
        });

        onMounted(() => {
            this._updatePreview();
            this._setupKeyboardShortcuts();
        });

        onWillUnmount(() => {
            this._cleanupKeyboardShortcuts();
        });
    }

    // ── Preview Management ────────────────────────────────────────────────────

    _updatePreview() {
        this.state.previewHtml = MarkdownParser.parse(this.state.value);
    }

    _onInput(ev) {
        this.state.value = ev.target.value;
        this._updatePreview();
        this.props.record.update({ [this.props.name]: this.state.value });
    }

    // ── Toolbar Actions ───────────────────────────────────────────────────────

    _insertText(before, after = '', placeholder = '') {
        const textarea = this.textareaRef.el;
        const start = textarea.selectionStart;
        const end = textarea.selectionEnd;
        const selectedText = textarea.value.substring(start, end);
        const replacement = before + (selectedText || placeholder) + after;
        
        const newValue = textarea.value.substring(0, start) + replacement + textarea.value.substring(end);
        this.state.value = newValue;
        this._updatePreview();
        this.props.record.update({ [this.props.name]: this.state.value });
        
        // Restore focus and selection
        textarea.focus();
        const newCursorPos = start + before.length + (selectedText || placeholder).length;
        textarea.setSelectionRange(newCursorPos, newCursorPos);
    }

    onBold() {
        this._insertText('**', '**', 'bold text');
    }

    onItalic() {
        this._insertText('*', '*', 'italic text');
    }

    onCode() {
        this._insertText('`', '`', 'code');
    }

    onCodeBlock() {
        this._insertText('```\n', '\n```', 'code block');
    }

    onHeader1() {
        this._insertText('# ', '', 'Header 1');
    }

    onHeader2() {
        this._insertText('## ', '', 'Header 2');
    }

    onHeader3() {
        this._insertText('### ', '', 'Header 3');
    }

    onLink() {
        this._insertText('[', '](url)', 'link text');
    }

    onList() {
        this._insertText('- ', '', 'list item');
    }

    // ── View Mode Management ──────────────────────────────────────────────────

    onModeEdit() {
        this.state.mode = 'edit';
    }

    onModePreview() {
        this.state.mode = 'preview';
    }

    onModeSplit() {
        this.state.mode = 'split';
    }

    // ── Keyboard Shortcuts ────────────────────────────────────────────────────

    _setupKeyboardShortcuts() {
        this._keydownHandler = (ev) => {
            if (ev.ctrlKey || ev.metaKey) {
                switch (ev.key) {
                    case 'b':
                        ev.preventDefault();
                        this.onBold();
                        break;
                    case 'i':
                        ev.preventDefault();
                        this.onItalic();
                        break;
                    case 'k':
                        ev.preventDefault();
                        this.onLink();
                        break;
                }
            }
        };
        
        if (this.textareaRef.el) {
            this.textareaRef.el.addEventListener('keydown', this._keydownHandler);
        }
    }

    _cleanupKeyboardShortcuts() {
        if (this.textareaRef.el && this._keydownHandler) {
            this.textareaRef.el.removeEventListener('keydown', this._keydownHandler);
        }
    }

    // ── Computed Properties ───────────────────────────────────────────────────

    get isEditMode() {
        return this.state.mode === 'edit';
    }

    get isPreviewMode() {
        return this.state.mode === 'preview';
    }

    get isSplitMode() {
        return this.state.mode === 'split';
    }

    get readonly() {
        return this.props.readonly;
    }
}

// ── Register Field Widget ────────────────────────────────────────────────────

registry.category("fields").add("markdown", MarkdownWidget);