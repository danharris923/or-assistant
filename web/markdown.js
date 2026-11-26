'use strict';
// Small, safe Markdown renderer: everything is HTML-escaped first, so model
// output can never inject markup or scripts. Covers what chat answers use.
window.renderMarkdown = (function () {
    const ESC = {'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'};
    const esc = s => s.replace(/[&<>"']/g, c => ESC[c]);

    function inline(src) {
        return src.split(/(`[^`\n]+`)/).map(part => {
            if (part.length > 1 && part[0] === '`' && part.endsWith('`')) return '<code>' + esc(part.slice(1, -1)) + '</code>';
            let t = esc(part);
            t = t.replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, (m, label, url) =>
                '<a href="' + url + '" target="_blank" rel="noopener noreferrer">' + label + '</a>');
            t = t.replace(/(^|[\s(])(https?:\/\/[^\s<)]+[^\s<).,;:!?'"])/g, (m, pre, url) =>
                pre + '<a href="' + url + '" target="_blank" rel="noopener noreferrer">' + url + '</a>');
            t = t.replace(/\*\*([^*\n]+?)\*\*/g, '<strong>$1</strong>');
            t = t.replace(/__([^_\n]+?)__/g, '<strong>$1</strong>');
            t = t.replace(/(^|[^*\w])\*([^*\s][^*\n]*?)\*(?![*\w])/g, '$1<em>$2</em>');
            t = t.replace(/~~([^~\n]+)~~/g, '<del>$1</del>');
            return t;
        }).join('');
    }

    const isFence = l => /^\s*(```|~~~)/.test(l);
    const isHeading = l => /^#{1,6}\s/.test(l);
    const isRule = l => /^\s*([-*_])(\s*\1){2,}\s*$/.test(l);
    const isUl = l => /^\s*[-*+]\s+/.test(l);
    const isOl = l => /^\s*\d+[.)]\s+/.test(l);
    const isQuote = l => /^\s*>/.test(l);
    const isTableSep = l => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(l);
    const cells = l => l.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map(c => c.trim());
    const startsBlock = (l, next) => isFence(l) || isHeading(l) || isRule(l) || isUl(l) || isOl(l) || isQuote(l) ||
        (l.includes('|') && next !== undefined && isTableSep(next));

    function render(src) {
        const lines = String(src).replace(/\r\n?/g, '\n').split('\n');
        let html = '', i = 0;
        while (i < lines.length) {
            const line = lines[i];
            if (!line.trim()) { i++; continue; }
            if (isFence(line)) {
                const marker = line.trim().slice(0, 3), buf = [];
                i++;
                while (i < lines.length && !lines[i].trim().startsWith(marker)) buf.push(lines[i++]);
                i++;
                html += '<pre><code>' + esc(buf.join('\n')) + '</code></pre>';
                continue;
            }
            if (isHeading(line)) {
                const m = line.match(/^(#{1,6})\s+(.*?)\s*#*\s*$/);
                const n = Math.min(m[1].length + 1, 6);
                html += '<h' + n + '>' + inline(m[2]) + '</h' + n + '>';
                i++; continue;
            }
            if (isRule(line)) { html += '<hr>'; i++; continue; }
            if (isUl(line) || isOl(line)) {
                const ordered = isOl(line), test = ordered ? isOl : isUl, items = [];
                const start = ordered ? parseInt(line.trim(), 10) : 1;
                while (i < lines.length) {
                    const l = lines[i];
                    if (test(l)) items.push(l.replace(ordered ? /^\s*\d+[.)]\s+/ : /^\s*[-*+]\s+/, ''));
                    else if (l.trim() && /^\s{2,}/.test(l) && !isFence(l)) items[items.length - 1] += '\n' + l.trim();
                    else if (!l.trim() && i + 1 < lines.length && test(lines[i + 1])) { i++; continue; }
                    else break;
                    i++;
                }
                const tag = ordered ? 'ol' : 'ul';
                html += '<' + tag + (ordered && start !== 1 ? ' start="' + start + '"' : '') + '>' +
                    items.map(t => '<li>' + inline(t.replace(/^\[ \]\s/, '☐ ').replace(/^\[x\]\s/i, '☑ ')).replace(/\n/g, '<br>') + '</li>').join('') +
                    '</' + tag + '>';
                continue;
            }
            if (isQuote(line)) {
                const buf = [];
                while (i < lines.length && isQuote(lines[i])) buf.push(lines[i++].replace(/^\s*>\s?/, ''));
                html += '<blockquote>' + render(buf.join('\n')) + '</blockquote>';
                continue;
            }
            if (line.includes('|') && i + 1 < lines.length && isTableSep(lines[i + 1])) {
                const head = cells(line);
                i += 2;
                let body = '';
                while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
                    body += '<tr>' + cells(lines[i]).map(c => '<td>' + inline(c) + '</td>').join('') + '</tr>';
                    i++;
                }
                html += '<table><thead><tr>' + head.map(c => '<th>' + inline(c) + '</th>').join('') + '</tr></thead><tbody>' + body + '</tbody></table>';
                continue;
            }
            const buf = [];
            while (i < lines.length && lines[i].trim() && !startsBlock(lines[i], lines[i + 1])) buf.push(lines[i++]);
            if (!buf.length) buf.push(lines[i++]);
            html += '<p>' + buf.map(inline).join('<br>') + '</p>';
        }
        return html;
    }
    return render;
})();
