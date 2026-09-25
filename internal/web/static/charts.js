/* LLM Gateway — dependency-free SVG charts.
   Public: Charts.bars({values:[{label,value}], color, height, format})
           Charts.sparkline({values:[...], color, height})
           Charts.barsMulti({series:[{name,color,values:[{label,value}]}], height})
   All render into a container element. Values may be 0/null (gaps). */
const Charts = (() => {

  function esc(s) { const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }

  function niceMax(v) {
    if (!v) return 1;
    const mag = Math.pow(10, Math.floor(Math.log10(v)));
    return Math.ceil(v / mag) * mag;
  }

  function fmtShort(v) {
    if (v == null) return '—';
    if (Math.abs(v) >= 1e9) return (v/1e9).toFixed(1) + 'B';
    if (Math.abs(v) >= 1e6) return (v/1e6).toFixed(1) + 'M';
    if (Math.abs(v) >= 1e3) return (v/1e3).toFixed(1) + 'k';
    return Math.round(v * 100) / 100 + '';
  }

  // Grouped bar chart. series: [{name,color,values:[{label,value}]}]
  function barsMulti({ series, height = 220, format = fmtShort, yLabel = '' }) {
    const all = series.flatMap(s => s.values);
    if (!all.length) return '<p class="muted">No data yet.</p>';
    const n = series[0].values.length;
    const max = niceMax(Math.max(...all.map(p => p.value || 0)));
    const W = Math.max(560, n * (series.length * 34 + 26) + 60);
    const H = height, padL = 52, padB = 30, padT = 12;
    const plotW = W - padL - 10, plotH = H - padB - padT;
    const groupW = plotW / n;
    const barW = Math.min(26, (groupW * 0.7) / series.length);

    let g = '';
    // horizontal gridlines (4)
    for (let i = 0; i <= 4; i++) {
      const y = padT + plotH - (plotH * i / 4);
      g += `<line class="grid-line" x1="${padL}" y1="${y}" x2="${W-10}" y2="${y}"/>`;
      g += `<text class="axis-text" x="${padL-6}" y="${y+3}" text-anchor="end">${format(max*i/4)}</text>`;
    }
    // bars + x labels
    series[0].values.forEach((p, i) => {
      const gx = padL + i * groupW + (groupW - series.length * barW) / 2;
      series.forEach((s, si) => {
        const v = s.values[i] ? (s.values[i].value || 0) : 0;
        const bh = max ? (v / max) * plotH : 0;
        const x = gx + si * barW;
        const y = padT + plotH - bh;
        g += `<rect class="chart-bar" x="${x}" y="${y}" width="${barW-2}" height="${Math.max(bh,1)}" rx="3" fill="${s.color}"><title>${esc(s.name)} — ${esc(p.label)}: ${format(v)}</title></rect>`;
      });
      g += `<text class="axis-text" x="${padL + i*groupW + groupW/2}" y="${H-10}" text-anchor="middle">${esc(p.label)}</text>`;
    });
    // baseline
    g += `<line class="axis-line" x1="${padL}" y1="${padT+plotH}" x2="${W-10}" y2="${padT+plotH}"/>`;

    const legend = '<div class="chart-legend">' + series.map(s =>
      `<span><i style="background:${s.color}"></i>${esc(s.name)}</span>`).join('') +
      (yLabel ? `<span style="margin-left:auto">${esc(yLabel)}</span>` : '') + '</div>';
    return legend + `<div class="chart-wrap"><svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}">${g}</svg></div>`;
  }

  // Single-metric sparkline (area + line)
  function sparkline({ values, color = '#3fb950', height = 60, format = fmtShort }) {
    const pts = values.filter(v => v != null);
    if (pts.length < 2) return '<p class="muted">Not enough data yet.</p>';
    const W = 300, H = height, max = niceMax(Math.max(...values));
    const step = W / (values.length - 1);
    const coords = values.map((v, i) => `${i*step},${H - (v/max)*H}`).join(' ');
    return `<div class="chart-wrap"><svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" style="height:${H}px">` +
      `<polygon points="0,${H} ${coords} ${W},${H}" fill="${color}" opacity="0.12"/>` +
      `<polyline points="${coords}" fill="none" stroke="${color}" stroke-width="2"/>` +
      `</svg><div class="muted">max ${format(max)}</div></div>`;
  }

  return { barsMulti, sparkline, fmtShort };
})();
