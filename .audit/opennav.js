(() => { const b=[...document.querySelectorAll('button')].find(x=>/open menu/i.test(x.getAttribute('aria-label')||'')); if(b){b.click(); return 'opened'} return 'no btn' })()
