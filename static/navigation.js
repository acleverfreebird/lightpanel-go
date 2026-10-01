export function matchNavigation(entries, query) {
  const terms = query.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  return entries.filter(entry => {
    const text = `${entry.id} ${entry.title} ${entry.description} ${entry.keywords || ''}`.toLocaleLowerCase();
    return terms.every(term => text.includes(term));
  });
}
